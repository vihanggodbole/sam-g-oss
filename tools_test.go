package sam

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func runTool(t *testing.T, tool Tool, ws string, limit int, args any) (string, string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := &Output{limit: limit, cancel: cancel}
	text, err := tool.Run(ctx, ToolCall{Workspace: ws, Arguments: json.RawMessage(mustJSON(args)), Output: out})
	return classifyCall(context.Background(), tool.Name, out, text, err, limit)
}

func builtin(name string, lock *sync.RWMutex) Tool {
	for _, t := range fileTools(lock) {
		if t.Name == name {
			return t
		}
	}
	panic(name)
}

func ed(oldText, newText string) map[string]any {
	return map[string]any{"old_text": oldText, "new_text": newText}
}

func TestCombinedResultBudget(t *testing.T) {
	classify := func(captured, text string, runErr error) (string, string) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		out := &Output{limit: 8, cancel: cancel}
		if _, err := out.Write([]byte(captured)); err != nil {
			t.Fatal(err)
		}
		return classifyCall(ctx, "custom", out, text, runErr, 8)
	}
	if status, res := classify("1234", "567", nil); status != "completed" || res != "1234\n567" {
		t.Fatalf("exactly at the combined limit: %s %q", status, res)
	}
	for _, c := range [][2]string{{"12345678", "metadata"}, {"1234", "5678"}, {"", "123456789"}} {
		if status, res := classify(c[0], c[1], nil); status != "failed" || !strings.Contains(res, "tool_output_limit") {
			t.Fatalf("%q + %q must fail: %s %q", c[0], c[1], status, res)
		}
	}
	// The descriptive error envelope keeps its own bound beyond the output budget.
	status, res := classify("12345678", "", errors.New(strings.Repeat("e", 100)))
	if status != "failed" || !strings.Contains(res, `"code":"tool_error"`) || !strings.Contains(res, "12345678") {
		t.Fatalf("error envelope: %s %q", status, res)
	}
}

func TestReadLimitSuggestsRange(t *testing.T) {
	ws := t.TempDir()
	var lines strings.Builder
	for i := range 100 {
		lines.WriteString("line " + strconv.Itoa(i+1) + "\n")
	}
	if err := os.WriteFile(filepath.Join(ws, "f.txt"), []byte(lines.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	read := builtin("read", &sync.RWMutex{})
	status, res := runTool(t, read, ws, 100, map[string]any{"path": "f.txt"})
	if status != "failed" || !strings.Contains(res, "tool_output_limit") || !strings.Contains(res, "offset") {
		t.Fatalf("%s %s", status, res)
	}
	status, res = runTool(t, read, ws, 100, map[string]any{"path": "f.txt", "offset": 10, "limit": 2})
	if status != "completed" || res != "line 10\nline 11\n" {
		t.Fatalf("range read: %s %q", status, res)
	}
}

func TestEditValidationLeavesFileUnchanged(t *testing.T) {
	ws := t.TempDir()
	p := filepath.Join(ws, "f.txt")
	orig := "alpha beta\ngamma beta\ndelta\naaa\n"
	if err := os.WriteFile(p, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	// Set the fixture's exact mode independently of the caller's umask.
	if err := os.Chmod(p, 0o640); err != nil {
		t.Fatal(err)
	}
	edit := builtin("edit", &sync.RWMutex{})
	cases := map[string][]map[string]any{
		"ambiguous":           {ed("delta", "D"), ed("beta", "B")},
		"overlap":             {ed("alpha beta", "x"), ed("a beta", "y")},
		"missing":             {ed("missing", "x")},
		"overlapping matches": {ed("aa", "X")}, // "aa" occurs at two offsets in "aaa"
		"no new_text":         {{"old_text": "delta"}},
		"null new_text":       {{"old_text": "delta", "new_text": nil}},
	}
	for name, edits := range cases {
		if status, res := runTool(t, edit, ws, 1000, map[string]any{"path": "f.txt", "edits": edits}); status != "failed" {
			t.Fatalf("%s: expected failure: %s", name, res)
		}
		if b, _ := os.ReadFile(p); string(b) != orig {
			t.Fatalf("%s: file changed after failed validation", name)
		}
	}
	status, res := runTool(t, edit, ws, 1000, map[string]any{"path": "f.txt", "edits": []map[string]any{
		ed("alpha", "ALPHA"), ed("delta\n", ""), // an explicit empty new_text deletes
	}})
	if b, _ := os.ReadFile(p); status != "completed" || string(b) != "ALPHA beta\ngamma beta\naaa\n" {
		t.Fatalf("%s %s %q", status, res, b)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o640 {
		t.Fatalf("permissions not preserved: %v", st.Mode())
	}
}

func TestWriteRequiresContentButAllowsEmpty(t *testing.T) {
	ws := t.TempDir()
	p := filepath.Join(ws, "f")
	write := builtin("write", &sync.RWMutex{})
	if err := os.WriteFile(p, []byte("precious"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range []map[string]any{{"path": "f"}, {"path": "f", "content": nil}} {
		status, res := runTool(t, write, ws, 1000, args)
		if b, _ := os.ReadFile(p); status != "failed" || string(b) != "precious" || !strings.Contains(res, "content is required") {
			t.Fatalf("%v: %s %s %q", args, status, res, b)
		}
	}
	if status, res := runTool(t, write, ws, 1000, map[string]any{"path": "f", "content": ""}); status != "completed" {
		t.Fatal(status, res)
	}
	if b, _ := os.ReadFile(p); len(b) != 0 {
		t.Fatalf("explicit empty content must truncate: %q", b)
	}
}
