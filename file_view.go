package sam

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"strings"
	"syscall"
	"unicode/utf8"
)

// DefaultFileViewLimit is the per-open human file byte budget, independent
// of model tool limits or session configuration.
const DefaultFileViewLimit = 8 << 20

// ReadFileRequest selects one complete human-owned file read. LimitBytes zero
// uses DefaultFileViewLimit; positive values select a per-open budget. Negative
// values are invalid. No session settings are changed or idle boundary needed.
type ReadFileRequest struct {
	SessionID  string
	Path       string
	LimitBytes int
}

// FileView is a bounded, user-requested text read from the runtime's machine.
// Path is absolute, cleaned, and retains symlinks so document-relative links
// resolve against the opened name. Text is not saved in history or events.
type FileView struct {
	SessionID  string `json:"session_id"`
	Path       string `json:"path"`
	Text       string `json:"text"`
	Bytes      int    `json:"bytes"`
	LimitBytes int    `json:"limit_bytes"`
}

// ReadFile reads a regular UTF-8 text file for a trusted human owner, not as a
// model tool. Relative paths use this session's workspace; absolute paths and
// symlinks may reach any file readable by the OS user, including sensitive files.
// LimitBytes bounds file bytes (not JSON overhead), independently of tools.
// No idle check, provider call, saved record, or runtime/file lock is involved.
// External changes during reading are not a transactional snapshot.
func (r *Runtime) ReadFile(ctx context.Context, req ReadFileRequest) (FileView, error) {
	if req.LimitBytes < 0 {
		return FileView{}, errorf(CodeInvalidArgument, "limit_bytes must be zero (backend default) or a positive integer byte count")
	}
	limit := req.LimitBytes
	if limit == 0 {
		limit = DefaultFileViewLimit
	}
	if err := r.readable(); err != nil {
		return FileView{}, err
	}
	s, err := loadSession(r.reader(ctx), req.SessionID)
	if err != nil {
		return FileView{}, readErr(ctx, err)
	}
	if strings.IndexByte(req.Path, 0) >= 0 || !utf8.ValidString(req.Path) {
		return FileView{}, errorf(CodeInvalidArgument, "path must be valid UTF-8 without NUL bytes")
	}
	p, err := resolvePath(s.workspace, req.Path)
	if err != nil {
		return FileView{}, errorf(CodeInvalidArgument, "%s", err)
	}
	text, err := readViewerText(ctx, p, limit)
	if err != nil {
		return FileView{}, err
	}
	return FileView{SessionID: req.SessionID, Path: p, Text: text, Bytes: len(text), LimitBytes: limit}, nil
}

func readViewerText(ctx context.Context, path string, limit int) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	// Check before opening devices, then check the descriptor too. NONBLOCK
	// prevents a replaced target from hanging on a FIFO. These are ordinary
	// owner-level checks, not protection against concurrent filesystem tampering.
	st, err := os.Stat(path)
	if err != nil {
		return "", fileReadError(path, err)
	}
	if !st.Mode().IsRegular() {
		return "", errorf(CodeUnsupported, "%s is not a regular file; only UTF-8 text files can be viewed", path)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", fileReadError(path, err)
	}
	defer f.Close()
	st, err = f.Stat()
	if err != nil {
		return "", fileReadError(path, err)
	}
	if !st.Mode().IsRegular() {
		return "", errorf(CodeUnsupported, "%s is no longer a regular file", path)
	}
	return captureViewerText(ctx, f, path, limit)
}

// captureViewerText retains at most limit bytes plus one overflow probe; file
// size metadata is not trusted, since a file can grow while it is being read.
func captureViewerText(ctx context.Context, reader io.Reader, path string, limit int) (string, error) {
	var data bytes.Buffer
	chunk := make([]byte, min(32<<10, limit))
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		// Avoid limit+1 arithmetic (the configurable limit can be MaxInt).
		next := min(len(chunk), limit-data.Len())
		if next == 0 {
			next = 1
		}
		n, err := reader.Read(chunk[:next])
		if n > limit-data.Len() {
			return "", errorf(CodeFileTooLarge, "file exceeds the selected per-open limit of %d bytes; no content was returned. Increase the file viewer limit and explicitly open the file again, or open a smaller file. Agent settings and running work do not need to change", limit)
		}
		data.Write(chunk[:n])
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", fileReadError(path, err)
		}
	}
	if !utf8.Valid(data.Bytes()) || hasBinaryControls(data.Bytes()) {
		return "", errorf(CodeUnsupported, "%s is not supported text; only valid UTF-8 without binary control bytes (except tab, CR and LF) can be viewed", path)
	}
	return data.String(), nil
}

func hasBinaryControls(data []byte) bool {
	for _, b := range data {
		if (b < 32 && b != '\t' && b != '\n' && b != '\r') || b == 127 {
			return true
		}
	}
	return false
}

func fileReadError(path string, err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return errorf(CodeNotFound, "file %s does not exist", path)
	}
	return errorf(CodeFileReadFailed, "cannot read file %s: %s", path, err)
}
