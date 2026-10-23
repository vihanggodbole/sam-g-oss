package sam

import (
	"encoding/json"
	"strings"
	"testing"
)

func msgItem(id, text string) map[string]any {
	return map[string]any{
		"id": id, "type": "message", "role": "assistant", "status": "completed",
		"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}},
	}
}

func TestSelectCutKeepsGroupsAndRecentBudget(t *testing.T) {
	item := func(seq int64, v map[string]any) histItem {
		return histItem{seq: seq, item: json.RawMessage(mustJSON(v))}
	}
	pad := strings.Repeat("x", 300)
	items := []histItem{
		item(1, userMessage("task "+pad)),
		item(2, map[string]any{"type": "reasoning", "encrypted_content": "e"}),
		item(3, map[string]any{"type": "function_call", "call_id": "a", "name": "bash", "arguments": "{}"}),
		item(4, map[string]any{"type": "function_call", "call_id": "b", "name": "bash", "arguments": "{}"}),
		item(5, map[string]any{"type": "function_call_output", "call_id": "a", "output": pad}),
		item(6, map[string]any{"type": "function_call_output", "call_id": "b", "output": pad}),
		item(7, msgItem("m", pad)),
	}
	if got := (&contextView{items: items}).selectCut(10000); got != 0 {
		t.Fatalf("everything fits: got cut %d", got)
	}
	// N is a minimum: a boundary inside the tool group expands backwards,
	// including the preceding reasoning, not forwards to discard the group.
	if got := (&contextView{items: items}).selectCut(estimateTokens(len(items[6].item) + len(items[5].item))); got != 1 {
		t.Fatalf("cut %d", got)
	}
	if got := (&contextView{items: items}).selectCut(1); got != 6 {
		t.Fatalf("tiny budget: %d", got)
	}
}

func TestMinimumRecentKeepsWholeOversizeAssistant(t *testing.T) {
	items := []histItem{
		{seq: 1, kind: kindUser, item: json.RawMessage(mustJSON(userMessage("older")))},
		{seq: 2, kind: kindOutput, item: json.RawMessage(mustJSON(msgItem("a", "older response")))},
		{seq: 3, kind: kindUser, item: json.RawMessage(mustJSON(userMessage("task")))},
		{seq: 4, kind: kindOutput, item: json.RawMessage(mustJSON(msgItem("b", strings.Repeat("x", 1000))))},
	}
	n := modelVisibleTokens(items[3].item) - 1
	if cut := (&contextView{items: items}).selectCut(n); cut != 3 {
		t.Fatalf("N+1 assistant must remain whole; cut=%d", cut)
	}
	// Exactly meeting N still keeps the same whole assistant, not zero items.
	if cut := (&contextView{items: items}).selectCut(n + 1); cut != 3 {
		t.Fatalf("exact N: cut=%d", cut)
	}
}

func TestMinimumRecentIgnoresUnreplayedReasoning(t *testing.T) {
	items := []histItem{
		{item: json.RawMessage(mustJSON(userMessage("older task")))},
		{item: json.RawMessage(mustJSON(msgItem("earlier", strings.Repeat("visible ", 200))))},
		{item: json.RawMessage(mustJSON(map[string]any{"type": "reasoning", "summary": strings.Repeat("private ", 10000)}))},
		{item: json.RawMessage(mustJSON(msgItem("latest", "short")))},
	}
	if cut := (&contextView{items: items}).selectCut(300); cut != 1 {
		t.Fatalf("unreplayed reasoning satisfied minimum: cut=%d, want earlier visible assistant", cut)
	}
}
