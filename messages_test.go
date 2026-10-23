package sam

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

const (
	testMessagesSignature = "synthetic-private-signature"
	testMessagesRedacted  = "synthetic-private-redacted-data"
)

func messagesTestModel() Model {
	return Model{Provider: "copilot", ModelID: copilotMessagesModel, API: APIAnthropicMessages, ContextTokens: 100000, MaxOutputTokens: 32000, ReasoningEfforts: []string{"low", "medium", "high", "xhigh", "max"}, ReasoningSummaries: true}
}

func messagesEvents(reason string, blocks ...map[string]any) []map[string]any {
	events := []map[string]any{{"type": "message_start", "message": map[string]any{"type": "message", "id": "synthetic-message", "role": "assistant", "content": []any{}, "usage": map[string]any{"input_tokens": 10, "cache_read_input_tokens": 3, "cache_creation_input_tokens": 2}}}}
	for i, b := range blocks {
		events = append(events, map[string]any{"type": "content_block_start", "index": i, "content_block": b}, map[string]any{"type": "content_block_stop", "index": i})
	}
	return append(events, map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": reason}, "usage": map[string]any{"output_tokens": 5, "output_tokens_details": map[string]any{"thinking_tokens": 1}}}, map[string]any{"type": "message_stop"})
}

func assertMessagesPrivate(t *testing.T, values ...string) {
	t.Helper()
	for _, value := range values {
		for _, secret := range []string{testMessagesSignature, testMessagesRedacted, `"native"`, `"signature"`} {
			if strings.Contains(value, secret) {
				t.Fatalf("private replay escaped: %s", value)
			}
		}
	}
}

func TestMessagesRequestSettingsAndUnsupportedInputs(t *testing.T) {
	m := resolvedModel{Alias: "claude", Model: messagesTestModel(), auth: AuthGitHubCopilot}
	base := modelRequest{model: m, instructions: "trusted", input: rawInput(userMessage("hello")), maxTokens: 8192}
	for _, effort := range append([]string{""}, messagesEfforts...) {
		req := base
		req.effort = effort
		for _, summary := range []bool{false, true} {
			req.summary = summary
			raw, err := messagesBody(req)
			if err != nil {
				t.Fatal(err)
			}
			var body map[string]any
			_ = json.Unmarshal(raw, &body)
			want := "omitted"
			if summary {
				want = "summarized"
			}
			if body["thinking"].(map[string]any)["display"] != want || body["thinking"].(map[string]any)["type"] != "adaptive" || (effort == "" && body["output_config"] != nil) {
				t.Fatal(body)
			}
		}
	}
	for _, mutate := range []func(*modelRequest){
		func(r *modelRequest) { r.effort = "off" }, func(r *modelRequest) { r.effort = "minimal" },
		func(r *modelRequest) { r.maxTokens = 0 }, func(r *modelRequest) { r.maxTokens = 32001 },
		func(r *modelRequest) {
			r.input = rawInput(map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_image", "image_url": "fake"}}})
		},
		func(r *modelRequest) {
			r.input = rawInput(map[string]any{"type": "reasoning", "encrypted_content": "old"})
		},
		func(r *modelRequest) { r.model.auth = "" }, func(r *modelRequest) { r.model.ModelID = "claude-haiku-4.5" },
	} {
		req := base
		mutate(&req)
		if _, err := messagesBody(req); err == nil {
			t.Fatal("unsupported request accepted", req)
		}
	}
	// A signature-only block must retain the required empty thinking string.
	raw := messagesOutput(messagesBlock{Type: "thinking", Signature: testMessagesSignature}, "group", false)
	var item outputItem
	if err := json.Unmarshal(raw, &item); err != nil {
		t.Fatal(err)
	}
	block, err := nativeBlock(item)
	if err != nil || !strings.Contains(mustJSON(block), `"thinking":""`) {
		t.Fatal(block, err)
	}
}

type brokenMessagesReader struct{}

func (brokenMessagesReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestMessagesNullableUsageRefusalAndBounds(t *testing.T) {
	for _, reason := range []string{"end_turn", "refusal", "max_tokens"} {
		events := messagesEvents(reason, map[string]any{"type": "text", "text": "Visible result"})
		events[0]["message"].(map[string]any)["usage"] = map[string]any{"input_tokens": nil}
		events[len(events)-2]["usage"] = map[string]any{"output_tokens": 7}
		var b strings.Builder
		for _, ev := range events {
			fmt.Fprintf(&b, "data: %s\n\n", mustJSON(ev))
		}
		res, err := decodeMessages(strings.NewReader(b.String()), DefaultMaxProviderEventBytes, func(string, string) {})
		if reason == "max_tokens" {
			var me *modelError
			if !errors.As(err, &me) || me.usage == nil || me.usage.OutputTokens == nil || *me.usage.OutputTokens != 7 {
				t.Fatal(err)
			}
			continue
		}
		if err != nil || res.usage.InputTokens != nil || res.usage.ReasoningTokens != nil || *res.usage.OutputTokens != 7 {
			t.Fatal(res, err)
		}
		var item outputItem
		_ = json.Unmarshal(res.output[0], &item)
		if (item.refusal() != "") != (reason == "refusal") {
			t.Fatal(item)
		}
	}
	if _, err := decodeMessages(strings.NewReader("data: "+strings.Repeat("x", 1024)+"\n\n"), 64, func(string, string) {}); err == nil {
		t.Fatal("event bound not enforced")
	}
}

func TestMessagesFragmentedToolInputMustNotReplaceInitialArguments(t *testing.T) {
	for _, partial := range []string{`{"replacement":true}`, `{"incomplete":`} {
		events := messagesEvents("tool_use", map[string]any{"type": "tool_use", "id": "call", "name": "read", "input": map[string]any{"path": "original"}})
		delta := map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "input_json_delta", "partial_json": partial}}
		events = append(append(append([]map[string]any{}, events[:2]...), delta), events[2:]...)
		var raw strings.Builder
		for _, ev := range events {
			fmt.Fprintf(&raw, "data: %s\n\n", mustJSON(ev))
		}
		if _, err := decodeMessages(strings.NewReader(raw.String()), DefaultMaxProviderEventBytes, func(string, string) {}); err == nil {
			t.Fatal("initial arguments silently replaced")
		}
	}
}

func TestMessagesTerminalMismatchPreservesKnownUsage(t *testing.T) {
	for _, reason := range []string{"end_turn", "refusal", "tool_use"} {
		block := map[string]any{"type": "tool_use", "id": "call", "name": "read", "input": map[string]any{}}
		if reason == "tool_use" {
			block = map[string]any{"type": "text", "text": "No call"}
		}
		var raw strings.Builder
		for _, ev := range messagesEvents(reason, block) {
			fmt.Fprintf(&raw, "data: %s\n\n", mustJSON(ev))
		}
		_, err := decodeMessages(strings.NewReader(raw.String()), DefaultMaxProviderEventBytes, func(string, string) {})
		var me *modelError
		if !errors.As(err, &me) || me.usage == nil || me.usage.OutputTokens == nil || *me.usage.OutputTokens != 5 {
			t.Fatalf("known terminal usage lost for %s: %v", reason, err)
		}
	}
}

func TestMessagesTerminalUsageSurvivesTransportFailure(t *testing.T) {
	events := messagesEvents("end_turn", map[string]any{"type": "text", "text": "answer"})
	var wire strings.Builder
	for _, event := range events[:len(events)-1] {
		fmt.Fprintf(&wire, "data: %s\n\n", mustJSON(event))
	}
	result, err := decodeMessages(io.MultiReader(strings.NewReader(wire.String()), brokenMessagesReader{}), DefaultMaxProviderEventBytes, func(string, string) {})
	if result != nil {
		t.Fatal("a transport failure must not authorize output")
	}
	var failure *modelError
	if !errors.As(err, &failure) || failure.usage == nil || failure.usage.OutputTokens == nil || *failure.usage.OutputTokens != 5 {
		t.Fatalf("provider reported terminal output usage=5, but error lost it: %T %v", err, err)
	}
}

func TestMessagesNativeRolesAdjacencyAndCompactionGroups(t *testing.T) {
	m := resolvedModel{Alias: "claude", Model: messagesTestModel(), auth: AuthGitHubCopilot}
	thinking := messagesOutput(messagesBlock{Type: "thinking", Thinking: "visible", Signature: testMessagesSignature}, "group", false)
	text := messagesOutput(messagesBlock{Type: "text", Text: strings.Repeat("prefix ", 100)}, "group", false)
	call := messagesOutput(messagesBlock{Type: "tool_use", ID: "call", Name: "read", Input: json.RawMessage(`{"path":"a"}`)}, "group", false)
	result := json.RawMessage(`{"type":"function_call_output","call_id":"call","output":"real result"}`)
	trusted := json.RawMessage(`{"type":"message","role":"developer","content":[{"type":"input_text","text":"trusted historical directive"}]}`)
	req := modelRequest{model: m, maxTokens: 100, instructions: "stable", outcomes: map[string]string{"call": "completed"}, input: []json.RawMessage{json.RawMessage(mustJSON(userMessage("user"))), thinking, text, call, trusted, result}}
	if _, err := messagesBody(req); ErrorCodeOf(err) != CodeHistoryIncompatible {
		t.Fatal("instructions silently reordered", err)
	}
	req.input = []json.RawMessage{trusted, json.RawMessage(mustJSON(userMessage("user"))), thinking, text, call, result, trusted}
	body, err := messagesBody(req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"role":"system"`) || !strings.Contains(string(body), testMessagesSignature) {
		t.Fatal("trusted roles or replay lost")
	}
	items := []histItem{{seq: 1, item: json.RawMessage(mustJSON(userMessage("user")))}, {seq: 2, item: thinking}, {seq: 3, item: text}, {seq: 4, item: call}, {seq: 5, item: result}}
	if cut := (&contextView{items: items}).selectCut(1); cut != 1 {
		t.Fatal("compaction split signed response/tool-result group", cut)
	}
}

func TestMessagesSafeValidationDiagnostics(t *testing.T) {
	for _, block := range []map[string]any{
		{"type": "image", "data": testMessagesRedacted},
		{"type": "text", "text": testMessagesSignature, "citations": []any{testMessagesRedacted}},
		{"type": "tool_use", "id": "id", "name": "effect", "input": map[string]any{}, "caller": map[string]any{"type": "server_tool", "data": testMessagesRedacted}},
		{"type": "thinking", "thinking": testMessagesSignature, "signature": testMessagesSignature, "future": testMessagesRedacted},
		{"type": "thinking", "signature": testMessagesSignature},
	} {
		var wire strings.Builder
		for _, ev := range messagesEvents("end_turn", block) {
			fmt.Fprintf(&wire, "data: %s\n\n", mustJSON(ev))
		}
		result, err := decodeMessages(strings.NewReader(wire.String()), DefaultMaxProviderEventBytes, func(string, string) {})
		if result != nil || err == nil {
			t.Fatal("unsupported block accepted")
		}
		if strings.Contains(err.Error(), "malformed Messages event") {
			t.Fatal("safe cause hidden", err)
		}
		assertMessagesPrivate(t, err.Error())
	}
	// Syntax/type errors must not print JSON values, unlike safe codec errors.
	_, err := decodeMessages(strings.NewReader(`data: {"type":"content_block_start","index":"`+testMessagesSignature+`"}`+"\n\n"), DefaultMaxProviderEventBytes, func(string, string) {})
	if err == nil {
		t.Fatal("invalid type accepted")
	}
	assertMessagesPrivate(t, err.Error())
}

func TestMessagesReplayHasSingleAuthoritativeContent(t *testing.T) {
	literal := ` {"large":900719925474099312345,"dup":1,"dup":2,"text":"a  b"} `
	blocks := []messagesBlock{
		{Type: "text", Text: "exact text\n"},
		{Type: "thinking", Thinking: "exact signed thinking\n\n", Signature: testMessagesSignature},
		{Type: "redacted_thinking", Data: testMessagesRedacted},
		{Type: "tool_use", ID: "exact.id", Name: "read", Input: json.RawMessage(literal), Caller: json.RawMessage(`{"type":"direct","extra":900719925474099312345}`)},
	}
	input := rawInput(userMessage("prompt"))
	for _, b := range blocks {
		raw := messagesOutput(b, "group", false)
		var item outputItem
		if err := json.Unmarshal(raw, &item); err != nil {
			t.Fatal(err)
		}
		rebuilt, err := nativeBlock(item)
		if err != nil || mustJSON(rebuilt) != mustJSON(b) {
			t.Fatalf("replay changed: %s / %s (%v)", mustJSON(b), mustJSON(rebuilt), err)
		}
		if b.Type == "tool_use" && item.Arguments != literal {
			t.Fatal("literal argument string changed")
		}
		if b.Type == "text" && strings.Count(string(raw), `exact text`) != 1 {
			t.Fatal("duplicate text")
		}
		if b.Type == "thinking" && strings.Count(string(raw), `exact signed thinking`) != 1 {
			t.Fatal("duplicate thinking")
		}
		input = append(input, raw)
	}
	input = append(input, json.RawMessage(mustJSON(map[string]string{"type": "function_call_output", "call_id": "exact.id", "output": "real result"})))
	req := modelRequest{model: resolvedModel{Model: messagesTestModel(), auth: AuthGitHubCopilot}, maxTokens: 8192, input: input, outcomes: map[string]string{"exact.id": "completed"}}
	body, err := messagesBody(req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"input":{"large":900719925474099312345,"dup":1,"dup":2,"text":"a  b"}`) {
		t.Fatal("literal wire arguments lost", string(body))
	}
	var decoded struct {
		Messages []struct{ Content []json.RawMessage }
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Messages) != 3 || len(decoded.Messages[1].Content) != 4 {
		t.Fatal("response grouping changed")
	}
	for i, b := range blocks {
		var actual messagesBlock
		if err := json.Unmarshal(decoded.Messages[1].Content[i], &actual); err != nil || mustJSON(actual) != mustJSON(b) {
			t.Fatal("wire block changed", i, err)
		}
	}
}

func TestMessagesPrivateResponseFieldsStayOutOfFailures(t *testing.T) {
	events := messagesEvents("end_turn", map[string]any{"type": "thinking", "thinking": "", "signature": testMessagesSignature}, map[string]any{"type": "redacted_thinking", "data": testMessagesRedacted})
	events = append(events[:len(events)-2], map[string]any{"type": "error", "error": map[string]any{"type": "invalid_request_error", "message": "invalid " + testMessagesSignature + " " + testMessagesRedacted}})
	var wire strings.Builder
	for _, ev := range events {
		fmt.Fprintf(&wire, "data: %s\n\n", mustJSON(ev))
	}
	_, err := decodeMessages(strings.NewReader(wire.String()), DefaultMaxProviderEventBytes, func(string, string) {})
	if err == nil {
		t.Fatal("error accepted")
	}
	var me *modelError
	if !errors.As(err, &me) {
		t.Fatal(err)
	}
	assertMessagesPrivate(t, sanitizeModelError(me).Error())
}

func TestMessagesFragmentedToolInputStaysLiteral(t *testing.T) {
	s := messagesStream{started: true, seen: map[string]bool{}, delta: func(string, string) {}}
	initial := messagesBlock{Type: "tool_use", ID: "id", Name: "read", Input: json.RawMessage(`{}`)}
	if err := s.startBlock(messagesEvent{Index: new(0), Block: &initial}); err != nil {
		t.Fatal(err)
	}
	literal := ` {"large":900719925474099312345,"dup":1,"dup":2,"text":"` + strings.Repeat("x", 256) + `"} `
	for i := 0; i < len(literal); i += 16 {
		var ev messagesEvent
		data := map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]string{"type": "input_json_delta", "partial_json": literal[i:min(i+16, len(literal))]}}
		if err := json.Unmarshal([]byte(mustJSON(data)), &ev); err != nil {
			t.Fatal(err)
		}
		if err := s.blockDelta(ev); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.stopBlock(messagesEvent{Index: new(0)}); err != nil {
		t.Fatal(err)
	}
	if string(s.blocks[0].Input) != literal {
		t.Fatal("fragmented literal input changed")
	}
	// A following block must get fresh buffers without changing the saved block.
	text := messagesBlock{Type: "text", Text: "next"}
	if err := s.startBlock(messagesEvent{Index: new(1), Block: &text}); err != nil {
		t.Fatal(err)
	}
	if err := s.stopBlock(messagesEvent{Index: new(1)}); err != nil {
		t.Fatal(err)
	}
	if string(s.blocks[0].Input) != literal || s.blocks[1].Text != "next" {
		t.Fatal("block lifecycle changed prior buffers")
	}
}
