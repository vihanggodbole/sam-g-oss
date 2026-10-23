package sam

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"
)

// Built-in file tools share one runtime-wide RWMutex: reads overlap,
// writes and edits are exclusive across validation and mutation.

func fileTools(lock *sync.RWMutex) []Tool {
	return []Tool{
		{
			Name:        "read",
			Description: "Read a UTF-8 text file. Relative paths resolve against the workspace. Use offset (1-based line) and limit (line count) to read part of a large file.",
			Parameters: json.RawMessage(`{"type":"object","properties":{` +
				`"path":{"type":"string","description":"File path"},` +
				`"offset":{"type":"integer","description":"First line to read, 1-based"},` +
				`"limit":{"type":"integer","description":"Maximum number of lines"}},"required":["path"]}`),
			Run: func(ctx context.Context, c ToolCall) (string, error) { return readTool(ctx, lock, c) },
		},
		{
			Name:         "write",
			ReplayUnsafe: true,
			Description:  "Create or replace a text file with the given content, creating missing parent directories.",
			Parameters: json.RawMessage(`{"type":"object","properties":{` +
				`"path":{"type":"string"},"content":{"type":"string"}},"required":["path","content"]}`),
			Run: func(ctx context.Context, c ToolCall) (string, error) { return writeTool(ctx, lock, c) },
		},
		{
			Name:         "edit",
			ReplayUnsafe: true,
			Description: "Apply exact text replacements to a file. Each old_text must match exactly once and edits must not overlap. " +
				"All edits are validated before the file is changed.",
			Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},` +
				`"edits":{"type":"array","items":{"type":"object","properties":{` +
				`"old_text":{"type":"string"},"new_text":{"type":"string"}},"required":["old_text","new_text"]}}},` +
				`"required":["path","edits"]}`),
			Run: func(ctx context.Context, c ToolCall) (string, error) { return editTool(ctx, lock, c) },
		},
	}
}

func decodeArgs(raw json.RawMessage, v any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

func resolvePath(workspace, p string) (string, error) {
	if p == "" {
		return "", errors.New("path is required")
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(workspace, p)
	}
	return filepath.Clean(p), nil
}

// regularTarget resolves symlinks and returns the target path and its mode.
// A missing file returns exists=false and the path with its existing parent
// directories resolved, which is where creating it will put it.
// Non-regular targets are rejected.
func regularTarget(p string) (target string, mode fs.FileMode, exists bool, err error) {
	target, err = filepath.EvalSymlinks(p)
	if errors.Is(err, fs.ErrNotExist) {
		target, err = resolveMissing(p)
		return target, 0o644, false, err
	}
	if err != nil {
		return "", 0, false, err
	}
	st, err := os.Stat(target)
	if err != nil {
		return "", 0, false, err
	}
	if !st.Mode().IsRegular() {
		return "", 0, false, fmt.Errorf("%s is not a regular file", p)
	}
	return target, st.Mode().Perm(), true, nil
}

// resolveMissing resolves the existing directory prefix of a path whose
// final component (and possibly some parents) does not exist yet.
func resolveMissing(p string) (string, error) {
	dir := filepath.Dir(p)
	if dir == p {
		return p, nil
	}
	real, err := filepath.EvalSymlinks(dir)
	if errors.Is(err, fs.ErrNotExist) {
		real, err = resolveMissing(dir)
	}
	if err != nil {
		return "", err
	}
	return filepath.Join(real, filepath.Base(p)), nil
}

func readTool(ctx context.Context, lock *sync.RWMutex, c ToolCall) (string, error) {
	var a struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := decodeArgs(c.Arguments, &a); err != nil {
		return "", err
	}
	p, err := resolvePath(c.Workspace, a.Path)
	if err != nil {
		return "", err
	}
	if a.Offset < 0 || a.Limit < 0 {
		return "", errors.New("offset and limit must be positive")
	}
	lock.RLock()
	defer lock.RUnlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	target, _, exists, err := regularTarget(p)
	if err != nil {
		return "", err
	}
	if !exists {
		return "", fmt.Errorf("%s does not exist", p)
	}
	f, err := os.Open(target)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return copyLines(ctx, bufio.NewReaderSize(f, 64<<10), c.Output, max(1, a.Offset), a.Limit)
}

// copyLines writes the selected lines. A NUL byte in the first 8 KiB marks
// a binary file; selected lines must be valid UTF-8.
func copyLines(ctx context.Context, br *bufio.Reader, out io.Writer, start, limit int) (string, error) {
	if head, _ := br.Peek(8 << 10); bytes.IndexByte(head, 0) >= 0 {
		return "", errors.New("file appears to be binary; read only supports text files")
	}
	line, wrote := 1, false
	var carry []byte // incomplete UTF-8 sequence split across buffer-sized chunks
	for limit == 0 || line < start+limit {
		chunk, err := br.ReadSlice('\n')
		if len(chunk) > 0 && line >= start {
			v := append(carry, chunk...)
			n := len(v)
			if errors.Is(err, bufio.ErrBufferFull) {
				n = completeUTF8(v)
			}
			if !utf8.Valid(v[:n]) {
				return "", fmt.Errorf("line %d is not valid UTF-8; read only supports UTF-8 text files", line)
			}
			if _, werr := out.Write(v[:n]); werr != nil {
				return "", werr
			}
			carry, wrote = append(carry[:0:0], v[n:]...), true
		}
		if len(chunk) > 0 && chunk[len(chunk)-1] == '\n' {
			line++
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil && !errors.Is(err, bufio.ErrBufferFull) {
			return "", err
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
	}
	if len(carry) > 0 {
		return "", fmt.Errorf("line %d ends with an incomplete UTF-8 sequence; read only supports UTF-8 text files", line)
	}
	if !wrote {
		return fmt.Sprintf("(no lines at offset %d; the file has %d lines)", start, line-1), nil
	}
	return "", nil
}

func writeTool(ctx context.Context, lock *sync.RWMutex, c ToolCall) (string, error) {
	var a struct {
		Path    string  `json:"path"`
		Content *string `json:"content"` // nil when missing or null
	}
	if err := decodeArgs(c.Arguments, &a); err != nil {
		return "", err
	}
	if a.Content == nil {
		return "", errors.New(`content is required; use "" to write an empty file`)
	}
	p, err := resolvePath(c.Workspace, a.Path)
	if err != nil {
		return "", err
	}
	lock.Lock()
	defer lock.Unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	target, mode, _, err := regularTarget(p)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return "", err
	}
	if err := replaceFile(target, []byte(*a.Content), mode); err != nil {
		return "", err
	}
	return fmt.Sprintf("Wrote %d bytes to %s", len(*a.Content), p), nil
}

type replacement struct {
	OldText string  `json:"old_text"`
	NewText *string `json:"new_text"` // nil when missing or null
}

func editTool(ctx context.Context, lock *sync.RWMutex, c ToolCall) (string, error) {
	var a struct {
		Path  string        `json:"path"`
		Edits []replacement `json:"edits"`
	}
	if err := decodeArgs(c.Arguments, &a); err != nil {
		return "", err
	}
	p, err := resolvePath(c.Workspace, a.Path)
	if err != nil {
		return "", err
	}
	if len(a.Edits) == 0 {
		return "", errors.New("edits must not be empty")
	}
	for i, e := range a.Edits {
		if e.NewText == nil {
			return "", fmt.Errorf(`edit %d: new_text is required; use "" to delete the matched text`, i+1)
		}
	}
	lock.Lock()
	defer lock.Unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	target, mode, exists, err := regularTarget(p)
	if err != nil {
		return "", err
	}
	if !exists {
		return "", fmt.Errorf("%s does not exist", p)
	}
	old, err := os.ReadFile(target)
	if err != nil {
		return "", err
	}
	updated, err := applyEdits(string(old), a.Edits)
	if err != nil {
		return "", fmt.Errorf("%w; the file was not changed", err)
	}
	if err := replaceFile(target, []byte(updated), mode); err != nil {
		return "", err
	}
	return fmt.Sprintf("Applied %d edit(s) to %s", len(a.Edits), p), nil
}

// applyEdits validates every replacement against the original text before
// building the result: each old_text must match exactly once and no two
// matches may overlap.
func applyEdits(text string, edits []replacement) (string, error) {
	type span struct{ start, end, i int }
	spans := make([]span, 0, len(edits))
	for i, e := range edits {
		if e.OldText == "" {
			return "", fmt.Errorf("edit %d: old_text is empty", i+1)
		}
		start := strings.Index(text, e.OldText)
		if start < 0 {
			return "", fmt.Errorf("edit %d: old_text was not found", i+1)
		}
		// Look for another occurrence starting one byte later, so that
		// overlapping matches ("aa" in "aaa") count as ambiguous too.
		if strings.Contains(text[start+1:], e.OldText) {
			return "", fmt.Errorf("edit %d: old_text must match exactly once, but it matches more than once; include more surrounding text", i+1)
		}
		spans = append(spans, span{start, start + len(e.OldText), i})
	}
	slices.SortFunc(spans, func(a, b span) int { return a.start - b.start })
	var sb strings.Builder
	pos := 0
	for k, s := range spans {
		if k > 0 && s.start < spans[k-1].end {
			return "", fmt.Errorf("edits %d and %d overlap", spans[k-1].i+1, s.i+1)
		}
		sb.WriteString(text[pos:s.start])
		sb.WriteString(*edits[s.i].NewText)
		pos = s.end
	}
	sb.WriteString(text[pos:])
	return sb.String(), nil
}

// replaceFile writes data to a same-directory temporary file and renames it
// over path, preserving the given permissions.
func replaceFile(path string, data []byte, mode fs.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".sam-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(data)
	if err == nil {
		err = f.Chmod(mode)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return err
}
