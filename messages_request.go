package sam

import (
	"encoding/json"
	"slices"
)

type messagesMessage struct {
	Role    string `json:"role"`
	Content []any  `json:"content"`
}

type messagesInput struct {
	system   []any
	messages []messagesMessage
	group    string
	pending  map[string]bool
	outcomes map[string]string
}

func messagesBody(req modelRequest) ([]byte, error) {
	if err := validateModelAPI(req.model.Model, Provider{Auth: req.model.auth}); err != nil {
		return nil, err
	}
	if err := validateMessagesCap(req.model, req.maxTokens); err != nil {
		return nil, err
	}
	if req.effort != "" && !slices.Contains(req.model.ReasoningEfforts, req.effort) {
		return nil, errorf(CodeUnsupported, "model %q does not declare reasoning effort %q", req.model.Alias, req.effort)
	}
	if req.summary && !req.model.ReasoningSummaries {
		return nil, errorf(CodeUnsupported, "model %q does not declare reasoning display support", req.model.Alias)
	}
	in := messagesInput{pending: map[string]bool{}, outcomes: req.outcomes}
	if req.instructions != "" {
		in.system = append(in.system, messagesText(req.instructions))
	}
	for _, raw := range req.input {
		var item outputItem
		if json.Unmarshal(raw, &item) != nil {
			return nil, messagesHistoryError("malformed saved input")
		}
		if err := in.add(item); err != nil {
			return nil, err
		}
	}
	if len(in.pending) > 0 {
		return nil, messagesHistoryError("an assistant tool-use message has no complete adjacent result batch")
	}
	if len(in.messages) == 0 {
		return nil, errorf(CodeInvalidArgument, "Messages requires conversation input")
	}
	display := "omitted"
	if req.summary {
		display = "summarized"
	}
	b := map[string]any{
		"model": req.model.ModelID, "messages": in.messages, "stream": true,
		"max_tokens": req.maxTokens, "thinking": map[string]string{"type": "adaptive", "display": display},
	}
	if len(in.system) > 0 {
		b["system"] = in.system
	}
	if req.effort != "" {
		b["output_config"] = map[string]string{"effort": req.effort}
	}
	if len(req.tools) > 0 {
		tools := make([]map[string]any, 0, len(req.tools))
		for _, t := range req.tools {
			tools = append(tools, map[string]any{"name": t.Name, "description": t.Description, "input_schema": t.Parameters})
		}
		b["tools"] = tools
	}
	// No Claude Code identity, fallback, binding-drop policy or unrelated betas.
	// No cache hints: the stable leading system/tools layout is not a hit promise.
	return json.Marshal(b)
}

func messagesText(text string) map[string]string {
	return map[string]string{"type": "text", "text": text}
}

func messagesHistoryError(detail string) error {
	return errorf(CodeHistoryIncompatible, "%s. Saved history is unchanged; explicitly switch the idle chat's model/account to use portable context, or start a new chat", detail)
}

func (in *messagesInput) add(item outputItem) error {
	if item.API != "" && item.API != APIResponses && item.API != APIAnthropicMessages {
		return messagesHistoryError("unknown saved protocol")
	}
	if item.API == APIAnthropicMessages {
		return in.addNative(item)
	}
	in.group = ""
	if item.Type == "function_call_output" {
		return in.addResult(item)
	}
	if len(in.pending) > 0 {
		return messagesHistoryError("instructions or messages interrupt the required tool-use/result adjacency; they cannot be reordered or demoted")
	}
	if item.Type != "message" {
		return messagesHistoryError("nonportable Responses reasoning or tool data cannot enter Messages")
	}
	blocks := make([]any, 0, len(item.Content))
	for _, c := range item.Content {
		switch c.Type {
		case "input_text", "output_text":
			blocks = append(blocks, messagesText(c.Text))
		case "input_image":
			mediaType, data, ok := splitDataURL(c.ImageURL)
			if !ok || item.Role != "user" {
				return messagesHistoryError("only images sent by the user as data can enter Messages")
			}
			blocks = append(blocks, map[string]any{"type": "image", "source": map[string]string{"type": "base64", "media_type": mediaType, "data": data}})
		case "refusal":
			if item.Role != "assistant" {
				return messagesHistoryError("refusal content in a non-assistant message")
			}
			blocks = append(blocks, messagesText(c.Refusal))
		default:
			return errorf(CodeUnsupported, "Messages currently supports text input only; unsupported content type %q was not sent", c.Type)
		}
	}
	role := item.Role
	if role == "developer" {
		role = "system" // application-owned instructions remain trusted instructions
	}
	switch role {
	case "system":
		if len(in.messages) == 0 {
			in.system = append(in.system, blocks...)
			return nil
		}
	case "user", "assistant":
	default:
		return errorf(CodeUnsupported, "Messages cannot faithfully represent role %q; input was not sent", item.Role)
	}
	in.messages = append(in.messages, messagesMessage{Role: role, Content: blocks})
	return nil
}

func (in *messagesInput) addNative(item outputItem) error {
	block, err := nativeBlock(item)
	if err != nil {
		return err
	}
	if in.group != item.Native.MessageID {
		if len(in.pending) > 0 {
			return messagesHistoryError("another assistant message interrupts tool-result adjacency")
		}
		in.messages = append(in.messages, messagesMessage{Role: "assistant", Content: []any{}})
		in.group = item.Native.MessageID
	}
	if block.Type == "tool_use" {
		if in.pending[block.ID] {
			return messagesHistoryError("duplicate native tool-use ID")
		}
		in.pending[block.ID] = true
	}
	last := &in.messages[len(in.messages)-1]
	last.Content = append(last.Content, block)
	return nil
}

func (in *messagesInput) addResult(item outputItem) error {
	if !in.pending[item.CallID] {
		return messagesHistoryError("tool result has no matching native tool-use ID")
	}
	status := in.outcomes[item.CallID]
	switch status {
	case "completed", "failed", "cancelled", "uncertain", "not_executed":
	default:
		return messagesHistoryError("native tool result has no known saved outcome")
	}
	delete(in.pending, item.CallID)
	block := map[string]any{"type": "tool_result", "tool_use_id": item.CallID, "content": item.Output, "is_error": status != "completed"}
	last := &in.messages[len(in.messages)-1]
	if last.Role != "user" {
		in.messages = append(in.messages, messagesMessage{Role: "user", Content: []any{block}})
	} else {
		last.Content = append(last.Content, block)
	}
	return nil
}

// nativeBlock reconstructs one wire block from the single authoritative copy
// of each field. Arguments stay literal JSON, never a map[string]any conversion.
func nativeBlock(item outputItem) (messagesBlock, error) {
	if item.Native == nil || item.Native.MessageID == "" {
		return messagesBlock{}, messagesHistoryError("native Messages replay data is missing")
	}
	n := item.Native
	if (n.Signature != "" && n.Type != "thinking") || (n.Data != "" && n.Type != "redacted_thinking") || (len(n.Caller) > 0 && n.Type != "tool_use") {
		return messagesBlock{}, messagesHistoryError("native replay data belongs to a different block type")
	}
	b := messagesBlock{Type: n.Type, Signature: n.Signature, Data: n.Data, Caller: n.Caller}
	switch b.Type {
	case "text":
		text, err := nativeText(item)
		if err != nil {
			return b, err
		}
		b.Text = text
	case "thinking":
		if item.Type != "reasoning" || len(item.Summary) != 1 {
			return b, messagesHistoryError("invalid native thinking item")
		}
		b.Thinking = item.Summary[0].Text
	case "redacted_thinking":
		if item.Type != "reasoning" || len(item.Summary) != 0 {
			return b, messagesHistoryError("invalid native redacted item")
		}
	case "tool_use":
		if item.Type != "function_call" {
			return b, messagesHistoryError("invalid native tool item")
		}
		b.ID, b.Name, b.Input = item.CallID, item.Name, json.RawMessage(item.Arguments)
	default:
		return b, messagesHistoryError("unsupported native replay block type")
	}
	if validateMessagesBlock(b) != nil || validateMessagesCaller(b.Caller) != nil {
		return b, messagesHistoryError("invalid native replay fields")
	}
	return b, nil
}

func nativeText(item outputItem) (string, error) {
	if item.Type != "message" || item.Role != "assistant" || len(item.Content) != 1 {
		return "", messagesHistoryError("invalid native text item")
	}
	c := item.Content[0]
	switch c.Type {
	case "output_text":
		return c.Text, nil
	case "refusal":
		return c.Refusal, nil
	default:
		return "", messagesHistoryError("invalid native text content")
	}
}
