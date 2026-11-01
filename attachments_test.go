package sam

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

var testPDF = []byte("%PDF-1.4\n1 0 obj << /Type /Pages /Kids [2 0 R 3 0 R] >> endobj\n2 0 obj << /Type /Page >> endobj\n3 0 obj << /Type /Page >> endobj\n%%EOF\n")

// A result without files is saved and replayed as the plain string it always was;
// one with files keeps its parts.
func TestCallOutputKeepsItsShape(t *testing.T) {
	for _, out := range []callOutput{
		{text: "plain"},
		resultOutput("see the file", []Attachment{{Name: "a.pdf", MediaType: "application/pdf", Data: testPDF}}),
	} {
		raw := mustJSON(map[string]any{"type": "function_call_output", "call_id": "c1", "output": out})
		var item outputItem
		if err := json.Unmarshal([]byte(raw), &item); err != nil {
			t.Fatal(err)
		}
		if again := mustJSON(map[string]any{"type": "function_call_output", "call_id": "c1", "output": item.Output}); again != raw {
			t.Errorf("round trip changed the item:\n%s\n%s", raw, again)
		}
	}
	if s := mustJSON(callOutput{text: "plain"}); s != `"plain"` {
		t.Errorf("plain output is %s, want a string", s)
	}
}

// Messages providers get the PDF as a document block inside the tool result;
// a model without file support gets a note instead.
func TestAttachedPDFForEachProvider(t *testing.T) {
	out := resultOutput("Invoice:", []Attachment{{Name: "Invoice 10045.pdf", MediaType: "application/pdf", Data: testPDF}})
	blocks, _ := out.messagesContent().([]any)
	if len(blocks) != 2 {
		t.Fatalf("got %d blocks, want text and document: %v", len(blocks), blocks)
	}
	doc := mustJSON(blocks[1])
	if !strings.Contains(doc, `"type":"document"`) || !strings.Contains(doc, `"media_type":"application/pdf"`) || !strings.Contains(doc, `"title":"Invoice 10045.pdf"`) {
		t.Errorf("document block: %s", doc)
	}
	raw := json.RawMessage(mustJSON(map[string]any{"type": "function_call_output", "call_id": "c1", "output": out}))
	stripped := string(withoutImages([]json.RawMessage{raw})[0])
	if strings.Contains(stripped, "base64") || !strings.Contains(stripped, `"output":"Invoice: [file Invoice 10045.pdf]"`) {
		t.Errorf("without files: %s", stripped)
	}
	if got := out.costAdjust() + len(mustJSON(out)); got > 3*imageCostBytes {
		t.Errorf("a two-page PDF counts as %d bytes of context, want about two pictures", got)
	}
}

// read attaches a PDF instead of refusing it as binary, and still reads text.
func TestReadAttachesPDF(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "invoice.pdf"), testPDF, 0o600); err != nil {
		t.Fatal(err)
	}
	var files []Attachment
	var lock sync.RWMutex
	call := ToolCall{Workspace: dir, Arguments: json.RawMessage(`{"path":"invoice.pdf"}`), attached: &files}
	text, err := readTool(context.Background(), &lock, call)
	if err != nil || len(files) != 1 || files[0].MediaType != "application/pdf" || !strings.Contains(text, "about 2 pages") {
		t.Fatalf("text %q, files %d, err %v", text, len(files), err)
	}
	if _, err := readTool(context.Background(), &lock, ToolCall{Workspace: dir, Arguments: json.RawMessage(`{"path":"invoice.pdf"}`)}); err == nil {
		t.Error("a call that cannot carry files attached one anyway")
	}
}
