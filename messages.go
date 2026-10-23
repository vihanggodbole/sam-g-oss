package sam

import (
	"encoding/json"
	"slices"
)

// The initial compatibility profile is exact-ID and Copilot-only. This is
// request-format evidence from Pi 0.87.1, not entitlement or executable limits.
const copilotMessagesModel = "claude-opus-5.5"

var messagesEfforts = []string{"low", "medium", "high", "xhigh", "max"}

func validateModelAPI(m Model, p Provider) error {
	if m.MaxOutputTokens < 0 {
		return errorf(CodeInvalidArgument, "max_output_tokens must be non-negative (0 means unknown)")
	}
	switch m.API.effective() {
	case APIResponses:
		return nil
	case APIAnthropicMessages:
		if p.Auth != AuthGitHubCopilot || m.ModelID != copilotMessagesModel {
			return errorf(CodeUnsupported, "anthropic_messages currently supports only Copilot claude-opus-5.5 adaptive thinking")
		}
		if m.Unavailable == "" && m.MaxOutputTokens == 0 {
			return errorf(CodeInvalidArgument, "Messages requires a verified positive max_output_tokens; Pi limits are advisory")
		}
		for _, effort := range m.ReasoningEfforts {
			if !slices.Contains(messagesEfforts, effort) {
				return errorf(CodeUnsupported, "Opus 5.5 adaptive thinking does not support effort %q", effort)
			}
		}
		return nil
	default:
		return errorf(CodeUnsupported, "unknown model API %q; choose responses or anthropic_messages", m.API)
	}
}

func validateMessagesCap(m resolvedModel, cap int) error {
	if m.API.effective() != APIAnthropicMessages {
		return nil
	}
	if cap <= 0 || m.MaxOutputTokens <= 0 || cap > m.MaxOutputTokens {
		return errorf(CodeInvalidArgument, "Messages model %q requires an output reserve between 1 and its verified max_output_tokens (%d); requested %d. Set compaction.output_reserve explicitly or repair the model definition; the cap is not clamped", m.Alias, m.MaxOutputTokens, cap)
	}
	return nil
}

// Native history keeps ordinary text/arguments in their canonical fields and
// only grouping and extra replay data here. Public SQL reads drop this field.
type messagesReplay struct {
	MessageID string          `json:"message_id"`
	Type      string          `json:"type"`
	Signature string          `json:"signature,omitempty"`
	Data      string          `json:"data,omitempty"`
	Caller    json.RawMessage `json:"caller,omitempty"`
}

type messagesBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	Signature string          `json:"signature,omitempty"`
	Data      string          `json:"data,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	Caller    json.RawMessage `json:"caller,omitempty"`
}

func responsesInput(input []json.RawMessage) ([]json.RawMessage, error) {
	out := make([]json.RawMessage, 0, len(input))
	for _, raw := range input {
		var item map[string]json.RawMessage
		if json.Unmarshal(raw, &item) != nil {
			return nil, errorf(CodeHistoryIncompatible, "malformed saved request item; history is unchanged")
		}
		if api := item["api"]; len(api) != 0 && string(api) != `"responses"` && string(api) != `""` {
			return nil, errorf(CodeHistoryIncompatible, "native Messages context cannot be sent to Responses; explicitly switch this idle chat's model to create a portable-history boundary")
		}

		out = append(out, raw)
	}
	return out, nil
}

func messagesSecrets(input []json.RawMessage) []string {
	var out []string
	for _, raw := range input {
		var item outputItem
		if json.Unmarshal(raw, &item) != nil || item.Native == nil {
			continue
		}
		out = append(out, item.Native.Signature, item.Native.Data)
	}
	return out
}

// Required blank thinking/text fields must survive replay (omitempty would
// turn signature-only thinking into a different native block).
func (b messagesBlock) MarshalJSON() ([]byte, error) {
	m := map[string]any{"type": b.Type}
	switch b.Type {
	case "text":
		m["text"] = b.Text
	case "thinking":
		m["thinking"], m["signature"] = b.Thinking, b.Signature
	case "redacted_thinking":
		m["data"] = b.Data
	case "tool_use":
		m["id"], m["name"], m["input"] = b.ID, b.Name, b.Input
		if len(b.Caller) > 0 {
			m["caller"] = b.Caller
		}
	}
	return json.Marshal(m)
}

// Only these explicit, payload-free validation errors may appear in diagnostics.
type messagesValidationError string

func (e messagesValidationError) Error() string { return string(e) }

func (b *messagesBlock) UnmarshalJSON(raw []byte) error {
	type wire messagesBlock
	var value wire
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &value) != nil || json.Unmarshal(raw, &fields) != nil {
		return messagesValidationError("malformed Messages block")
	}
	allowed := map[string]bool{"type": true}
	var required []string
	switch value.Type {
	case "text":
		required = []string{"text"}
		allowed["citations"] = true
		if c := string(fields["citations"]); c != "" && c != "null" && c != "[]" {
			return messagesValidationError("Messages citations are unsupported")
		}
	case "thinking":
		required = []string{"thinking"}
		allowed["signature"] = true
	case "redacted_thinking":
		required = []string{"data"}
	case "tool_use":
		required = []string{"id", "name", "input"}
		allowed["caller"] = true
		if err := validateMessagesCaller(value.Caller); err != nil {
			return err
		}
	default:
		return messagesValidationError("unsupported Messages block")
	}
	for _, key := range required {
		allowed[key] = true
		if len(fields[key]) == 0 || string(fields[key]) == "null" {
			return messagesValidationError("Messages block missing required field: " + key)
		}
	}
	for key := range fields {
		if !allowed[key] {
			return messagesValidationError("unsupported Messages block field")
		}
	}
	*b = messagesBlock(value)
	return nil
}

func validateMessagesCaller(raw json.RawMessage) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var caller struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &caller) != nil || caller.Type != "direct" {
		return messagesValidationError("server tool invocation is unsupported")
	}
	return nil
}
