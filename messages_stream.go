package sam

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strings"
	"unicode/utf8"
)

type messagesUsage struct {
	Input      *int64 `json:"input_tokens"`
	Output     *int64 `json:"output_tokens"`
	CacheRead  *int64 `json:"cache_read_input_tokens"`
	CacheWrite *int64 `json:"cache_creation_input_tokens"`
	Details    *struct {
		Thinking *int64 `json:"thinking_tokens"`
	} `json:"output_tokens_details"`
}

type messagesEvent struct {
	Type    string `json:"type"`
	Index   *int   `json:"index"`
	Message *struct {
		ID      string          `json:"id"`
		Type    string          `json:"type"`
		Role    string          `json:"role"`
		Content []messagesBlock `json:"content"`
		Usage   *messagesUsage  `json:"usage"`
	} `json:"message"`
	Block *messagesBlock `json:"content_block"`
	Delta *struct {
		Type       string  `json:"type"`
		Text       *string `json:"text"`
		Thinking   *string `json:"thinking"`
		Signature  *string `json:"signature"`
		Partial    *string `json:"partial_json"`
		StopReason *string `json:"stop_reason"`
	} `json:"delta"`
	Usage *messagesUsage `json:"usage"`
	Error *wireFailure   `json:"error"`
}

type messagesStream struct {
	started   bool
	open      bool
	stopped   bool
	blocks    []messagesBlock
	input     strings.Builder
	text      strings.Builder
	signature strings.Builder
	seen      map[string]bool
	reason    string
	usage     messagesUsage
	delta     func(string, string)
}

func decodeMessages(body io.Reader, maxEvent int, delta func(string, string)) (*modelResult, error) {
	s := messagesStream{delta: delta, seen: map[string]bool{}}
	br := bufio.NewReaderSize(body, 64<<10)
	for {
		raw, err := readProviderEvent(br, maxEvent)
		if err != nil {
			return nil, s.failure(err)
		}
		var ev messagesEvent
		if !utf8.Valid(raw) {
			return nil, s.failure(messagesStreamError("Messages event contains invalid UTF-8"))
		}
		if err := json.Unmarshal(raw, &ev); err != nil {
			var safe messagesValidationError
			if errors.As(err, &safe) {
				return nil, s.failure(messagesStreamError(safe.Error()))
			}
			return nil, s.failure(messagesStreamError("malformed Messages event JSON"))
		}
		if ev.Type == "" {
			return nil, s.failure(messagesStreamError("Messages event type is missing"))
		}
		res, err := s.event(ev)
		if res != nil || err != nil {
			return res, s.failure(err)
		}
	}
}

func (s *messagesStream) failure(err error) error {
	if err == nil {
		return nil
	}
	var me *modelError
	if !errors.As(err, &me) {
		me = &modelError{msg: "reading response stream: " + err.Error(), retryable: true, cause: err}
	}
	if s.stopped {
		if u, usageErr := s.terminalUsage(); usageErr == nil {
			me.usage = &u
		}
	}
	me.secrets = append(me.secrets, s.signature.String())
	for _, b := range s.blocks {
		me.secrets = append(me.secrets, b.Signature, b.Data)
	}
	return me // HTTP owner redacts all request/response secrets before any truncation
}

func messagesStreamError(detail string) *modelError {
	return &modelError{msg: detail + "; unconfirmed output and tool calls were discarded"}
}

func (s *messagesStream) event(ev messagesEvent) (*modelResult, error) {
	switch ev.Type {
	case "ping":
		return nil, nil
	case "error":
		return nil, messagesProviderError(ev)
	case "message_start":
		return nil, s.startMessage(ev)
	case "content_block_start":
		return nil, s.startBlock(ev)
	case "content_block_delta":
		return nil, s.blockDelta(ev)
	case "content_block_stop":
		return nil, s.stopBlock(ev)
	case "message_delta":
		return nil, s.messageDelta(ev)
	case "message_stop":
		if !s.started || s.open || !s.stopped || s.reason == "" {
			return nil, messagesStreamError("Messages message_stop without complete terminal state")
		}
		return s.complete()
	default:
		return nil, messagesStreamError("unsupported Messages event type")
	}
}

func messagesProviderError(ev messagesEvent) error {
	if ev.Error == nil || ev.Error.Type == "" || ev.Error.Message == "" {
		return messagesStreamError("malformed Messages provider error")
	}
	e := providerModelError(&ProviderFailure{Code: ev.Error.Type, Message: ev.Error.Message, Param: ev.Error.Param, Shape: "error"})
	e.incompatible = false // native rejections stay structured provider failures, never prose heuristics
	return e
}

func (s *messagesStream) startMessage(ev messagesEvent) error {
	if s.started || ev.Message == nil || ev.Message.ID == "" || ev.Message.Type != "message" || ev.Message.Role != "assistant" || ev.Message.Content == nil || len(ev.Message.Content) != 0 {
		return messagesStreamError("invalid Messages message_start")
	}
	s.started = true
	return s.mergeUsage(ev.Message.Usage)
}

func (s *messagesStream) messageDelta(ev messagesEvent) error {
	if !s.started || s.open || s.stopped || ev.Delta == nil {
		return messagesStreamError("invalid Messages message_delta ordering")
	}
	if err := s.mergeUsage(ev.Usage); err != nil {
		return err
	}
	if ev.Delta.StopReason != nil {
		s.reason, s.stopped = *ev.Delta.StopReason, true
	}
	return nil
}

func (s *messagesStream) startBlock(ev messagesEvent) error {
	if !s.started || s.stopped || s.open || ev.Index == nil || *ev.Index != len(s.blocks) || ev.Block == nil {
		return messagesStreamError("invalid Messages content_block_start")
	}
	b := *ev.Block
	s.text.Reset()
	s.signature.Reset()
	s.input.Reset()
	switch b.Type {
	case "text":
		s.text.WriteString(b.Text)
		s.delta("text", b.Text)
	case "thinking":
		s.text.WriteString(b.Thinking)
		s.signature.WriteString(b.Signature)
		s.delta("reasoning", b.Thinking)
	case "redacted_thinking":
		if b.Data == "" {
			return messagesStreamError("redacted thinking has no private payload")
		}
	case "tool_use":
		if b.ID == "" || b.Name == "" || s.seen[b.ID] || !messagesObjectInput(b.Input) {
			return messagesStreamError("invalid or duplicate Messages tool-use identity")
		}
		s.seen[b.ID] = true
	default:
		return messagesStreamError("unsupported Messages content block")
	}
	s.open = true
	s.blocks = append(s.blocks, b)
	return nil
}

func (s *messagesStream) current(ev messagesEvent) (*messagesBlock, error) {
	if !s.open || s.stopped || ev.Index == nil || *ev.Index != len(s.blocks)-1 {
		return nil, messagesStreamError("Messages block event has no matching open block")
	}
	return &s.blocks[len(s.blocks)-1], nil
}

func (s *messagesStream) blockDelta(ev messagesEvent) error {
	b, err := s.current(ev)
	if err != nil {
		return err
	}
	if ev.Delta == nil {
		return messagesStreamError("Messages block delta is missing")
	}
	d := ev.Delta
	switch d.Type {
	case "text_delta":
		if b.Type != "text" || d.Text == nil {
			return messagesStreamError("invalid text delta")
		}
		s.text.WriteString(*d.Text)
		s.delta("text", *d.Text)
	case "thinking_delta":
		if b.Type != "thinking" || d.Thinking == nil {
			return messagesStreamError("invalid thinking delta")
		}
		s.text.WriteString(*d.Thinking)
		s.delta("reasoning", *d.Thinking)
	case "signature_delta":
		if b.Type != "thinking" || d.Signature == nil {
			return messagesStreamError("invalid private signature delta")
		}
		s.signature.WriteString(*d.Signature)
	case "input_json_delta":
		return s.inputDelta(b, d.Partial)
	default:
		return messagesStreamError("unsupported Messages block delta")
	}
	return nil
}

func messagesObjectInput(raw json.RawMessage) bool {
	var object map[string]json.RawMessage
	return json.Unmarshal(raw, &object) == nil && object != nil
}

func (s *messagesStream) inputDelta(b *messagesBlock, partial *string) error {
	if b.Type != "tool_use" || partial == nil {
		return messagesStreamError("invalid tool input delta")
	}
	if s.input.Len() == 0 {
		var initial map[string]json.RawMessage
		if json.Unmarshal(b.Input, &initial) != nil || len(initial) != 0 {
			return messagesStreamError("fragmented tool input cannot replace a nonempty initial object")
		}
	}
	s.input.WriteString(*partial)
	return nil
}

func (s *messagesStream) stopBlock(ev messagesEvent) error {
	b, err := s.current(ev)
	if err != nil {
		return err
	}
	switch b.Type {
	case "text":
		b.Text = s.text.String()
	case "thinking":
		b.Thinking, b.Signature = s.text.String(), s.signature.String()
	case "tool_use":
		if s.input.Len() > 0 {
			b.Input = json.RawMessage(s.input.String())
		}
	}
	if err := validateMessagesBlock(*b); err != nil {
		return err
	}
	s.open = false
	return nil
}

func validateMessagesBlock(b messagesBlock) error {
	switch b.Type {
	case "text":
	case "thinking":
		if strings.TrimSpace(b.Signature) == "" {
			return messagesStreamError("complete thinking block has no signature")
		}
	case "redacted_thinking":
		if b.Data == "" {
			return messagesStreamError("redacted thinking has no payload")
		}
	case "tool_use":
		if b.ID == "" || b.Name == "" || !messagesObjectInput(b.Input) {
			return messagesStreamError("complete tool-use block has invalid identity or object input")
		}
	default:
		return messagesStreamError("unsupported native replay block")
	}
	return nil
}

func (s *messagesStream) mergeUsage(u *messagesUsage) error {
	if u == nil {
		return nil
	}
	for _, n := range []*int64{u.Input, u.Output, u.CacheRead, u.CacheWrite} {
		if n != nil && *n < 0 {
			return messagesStreamError("invalid negative Messages usage")
		}
	}
	if u.Details != nil && u.Details.Thinking != nil && *u.Details.Thinking < 0 {
		return messagesStreamError("invalid Messages reasoning usage")
	}
	if u.Input != nil {
		s.usage.Input = u.Input
	}
	if u.Output != nil {
		s.usage.Output = u.Output
	}
	if u.CacheRead != nil {
		s.usage.CacheRead = u.CacheRead
	}
	if u.CacheWrite != nil {
		s.usage.CacheWrite = u.CacheWrite
	}
	if u.Details != nil && u.Details.Thinking != nil {
		s.usage.Details = u.Details
	}
	return nil
}

func (s *messagesStream) complete() (*modelResult, error) {
	u, err := s.terminalUsage()
	if err != nil {
		return nil, err
	}
	switch s.reason {
	case "end_turn", "refusal":
		if len(s.seen) > 0 {
			return nil, messagesStreamError("non-tool completion contains tool calls")
		}
	case "tool_use":
		if len(s.seen) == 0 {
			return nil, messagesStreamError("tool-use completion contains no calls")
		}
	case "max_tokens":
		e := messagesStreamError("Messages reached the requested hard output cap (max_tokens), including thinking. Increase compaction.output_reserve within this model's verified maximum output before continuing")
		e.usage = &u
		return nil, e
	default:
		detail := "Messages stopped with an unsupported stop reason"
		if s.reason == "pause_turn" || s.reason == "stop_sequence" {
			detail = "Messages stopped with " + s.reason
		}
		e := messagesStreamError(detail)
		e.usage = &u
		return nil, e
	}
	if len(s.blocks) == 0 {
		return nil, messagesStreamError("Messages completed without output blocks")
	}
	res := &modelResult{usage: u}
	group := newID("message")
	for _, b := range s.blocks {
		res.output = append(res.output, messagesOutput(b, group, s.reason == "refusal"))
	}
	return res, nil
}

func messagesOutput(b messagesBlock, group string, refused bool) json.RawMessage {
	item := map[string]any{"api": APIAnthropicMessages, "native": messagesReplay{MessageID: group, Type: b.Type, Signature: b.Signature, Data: b.Data, Caller: b.Caller}}
	switch b.Type {
	case "text":
		item["type"], item["role"] = "message", "assistant"
		content := map[string]string{"type": "output_text", "text": b.Text}
		if refused {
			content = map[string]string{"type": "refusal", "refusal": b.Text}
		}
		item["content"] = []any{content}
	case "thinking", "redacted_thinking":
		item["type"] = "reasoning"
		if b.Type == "thinking" {
			item["summary"] = []any{map[string]string{"type": "summary_text", "text": b.Thinking}}
		}
	case "tool_use":
		item["type"], item["call_id"], item["name"], item["arguments"] = "function_call", b.ID, b.Name, string(b.Input)
	}
	return json.RawMessage(mustJSON(item))
}

func (s *messagesStream) terminalUsage() (Usage, error) {
	u := Usage{OutputTokens: s.usage.Output, CacheReadTokens: s.usage.CacheRead, CacheWriteTokens: s.usage.CacheWrite}
	// Anthropic's ordinary input counter excludes cached input. Our input
	// counter includes it; unknown constituents leave the total unknown.
	if s.usage.Input != nil && s.usage.CacheRead != nil && s.usage.CacheWrite != nil {
		n := *s.usage.Input
		for _, extra := range []int64{*s.usage.CacheRead, *s.usage.CacheWrite} {
			if extra > math.MaxInt64-n {
				return Usage{}, messagesStreamError("Messages input usage overflow")
			}
			n += extra
		}
		if n < 0 {
			return Usage{}, messagesStreamError("Messages input usage overflow")
		}
		u.InputTokens = &n
	}
	if s.usage.Details != nil {
		u.ReasoningTokens = s.usage.Details.Thinking
	}
	if err := u.validate(); err != nil {
		return Usage{}, err
	}
	return u, nil
}
