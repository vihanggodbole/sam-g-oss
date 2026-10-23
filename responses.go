package sam

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Usage holds nullable token counters; nil means the provider did not report it.
type Usage struct {
	InputTokens      *int64 `json:"input_tokens"` // Total input, including cache reads/writes.
	OutputTokens     *int64 `json:"output_tokens"`
	ReasoningTokens  *int64 `json:"reasoning_tokens"`
	CacheReadTokens  *int64 `json:"cache_read_tokens"`
	CacheWriteTokens *int64 `json:"cache_write_tokens"`
}

func (u Usage) validate() error {
	for _, n := range []*int64{u.InputTokens, u.OutputTokens, u.ReasoningTokens, u.CacheReadTokens, u.CacheWriteTokens} {
		if n != nil && *n < 0 {
			return &modelError{msg: "invalid negative provider usage"}
		}
	}
	if u.OutputTokens != nil && u.ReasoningTokens != nil && *u.ReasoningTokens > *u.OutputTokens {
		return &modelError{msg: "reasoning usage exceeds output usage"}
	}
	var cached int64
	for _, n := range []*int64{u.CacheReadTokens, u.CacheWriteTokens} {
		if n == nil {
			continue
		}
		if *n > math.MaxInt64-cached {
			return &modelError{msg: "cache usage overflow"}
		}
		cached += *n
	}
	if u.InputTokens != nil && cached > *u.InputTokens {
		return &modelError{msg: "cache usage exceeds total input usage"}
	}
	return nil
}

// modelClient owns HTTP/auth for both codecs, main turns and compaction.
type modelClient struct {
	http     *http.Client
	auth     *AuthManager    // subscription credentials; nil without logins
	backoff  []time.Duration // waits before retries 1..3 without Retry-After
	maxEvent int             // bound on one assembled SSE event, in bytes
}

type modelRequest struct {
	sessionID    string // stable Copilot request/cache affinity, not credentials
	model        resolvedModel
	instructions string
	input        []json.RawMessage
	outcomes     map[string]string // actual saved tool-result outcomes; not Responses wire data
	tools        []Tool
	effort       string
	summary      bool
	maxTokens    int // operation output reserve; Messages hard cap only
	// initiator is sent to Copilot as X-Initiator: "user" or "agent". Empty
	// derives it from the last input item.
	initiator string
}

type modelResult struct {
	output   []json.RawMessage
	usage    Usage
	dispatch map[string]string // call_id -> validated local name; empty means unavailable
}

type streamHooks struct {
	// delta publishes visible answer text ("text"), refusal text
	// ("refusal"), and reasoning summary ("reasoning"). Nil means the stream is private (a compaction candidate):
	// nothing escapes, so a failed partial stream may still be retried.
	delta func(kind, text string)
}

// modelError is a provider failure with a retry classification.
type modelError struct {
	msg        string
	retryable  bool
	retryAfter time.Duration // valid Retry-After, or 0
	// rejected is the subscription access token the provider refused with
	// HTTP 401; the next attempt refreshes it once.
	rejected string
	// incompatible marks a rejection of saved history, such as opaque
	// reasoning the selected account cannot read.
	incompatible bool
	authRequired bool
	usage        *Usage   // known terminal failure usage, never incomplete output
	cause        error    // preserve transport/cancellation identity while carrying usage
	secrets      []string // current codec replay data; consumed by the shared diagnostic boundary
	failure      *ProviderFailure
}

func (e *modelError) Error() string { return e.msg }
func (e *modelError) Unwrap() []error {
	out := []error{&Error{Code: CodeProviderError, Message: e.msg, ProviderFailure: e.failure}}
	if e.cause != nil {
		out = append(out, e.cause)
	}
	return out
}

func (c *modelClient) body(req modelRequest) ([]byte, error) {
	switch req.model.API.effective() {
	case APIResponses:
		return responsesBody(req)
	case APIAnthropicMessages:
		return messagesBody(req)
	default:
		return nil, errorf(CodeUnsupported, "unsupported model API %q", req.model.API)
	}
}

func responsesBody(req modelRequest) ([]byte, error) {
	input, err := responsesInput(req.input)
	if err != nil {
		return nil, err
	}
	req.input = input
	if req.model.auth == AuthOpenAIChatGPT {
		for _, raw := range req.input {
			var item outputItem
			if json.Unmarshal(raw, &item) == nil && item.Role == "system" {
				return nil, errorf(CodeHistoryIncompatible, "SIWC does not accept explicit system message items. Saved history is unchanged; use a new session rather than converting its roles")
			}
		}
	}
	b := map[string]any{
		"model":        req.model.ModelID,
		"instructions": req.instructions,
		"input":        req.input,
		"stream":       true,
		"store":        false,
	}
	if req.model.auth == AuthGitHubCopilot && req.sessionID != "" {
		b["prompt_cache_key"] = req.sessionID
	}
	if len(req.tools) > 0 {
		tools := make([]map[string]any, 0, len(req.tools))
		for _, t := range req.tools {
			tools = append(tools, map[string]any{"type": "function", "name": t.Name, "description": t.Description, "parameters": t.Parameters})
		}
		if req.model.auth == AuthOpenAIChatGPT || req.model.auth == AuthGitHubCopilot {
			for _, t := range tools {
				t["strict"] = false
			}
		}
		if req.model.auth == AuthOpenAIChatGPT {
			b["tools"] = []map[string]any{{"type": "namespace", "name": "sam", "description": "Tools executed by sam-g on the agent host.", "tools": tools}}
		} else {
			b["tools"] = tools
		}
	}
	reasoning := map[string]any{}
	if req.effort != "" {
		reasoning["effort"] = req.effort
	}
	if req.summary {
		reasoning["summary"] = "auto"
	}
	if len(reasoning) > 0 {
		b["reasoning"] = reasoning
	}
	if req.model.supportsReasoning() {
		b["include"] = []string{"reasoning.encrypted_content"}
	}
	return json.Marshal(b)
}

func (c *modelClient) once(ctx context.Context, req modelRequest, body []byte, h streamHooks, rejected *string) (*modelResult, error) {
	if ctx.Err() != nil {
		return nil, context.Cause(ctx)
	}
	ep, err := c.endpoint(ctx, req, *rejected)
	if err != nil {
		return nil, err
	}
	*rejected = "" // resolution succeeded, even if the provider returned the same token
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, ep.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hr.Header = ep.header
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("Accept", "text/event-stream")
	hc := c.http
	if ep.subscription {
		hc = noRedirectClient(hc)
	}
	if req.model.auth == AuthOpenAIChatGPT {
		if err := c.auth.admitPlan(req.model.account); err != nil {
			return nil, err
		}
	}
	resp, err := hc.Do(hr)
	if err != nil {
		if identifiableCancellation(ctx, err) {
			return nil, context.Cause(ctx)
		}
		if ep.subscription {
			return nil, &modelError{msg: "subscription request transport failed", retryable: true}
		}
		return nil, &modelError{msg: diagnostic(fmt.Sprintf("request to %s failed: %v", ep.url, err), ep.secrets...), retryable: true}
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		e := statusErrorForAPI(resp, req.model.API.effective(), ep.secrets...)
		if resp.StatusCode == http.StatusUnauthorized && req.model.auth == AuthGitHubCopilot {
			e.retryable, e.rejected = true, ep.token
		}
		if req.model.auth == AuthOpenAIChatGPT {
			return nil, c.auth.recordPlanFailure(req.model.account, e)
		}
		return nil, e
	}
	if h.delta == nil {
		h.delta = func(string, string) {}
	}
	var res *modelResult
	switch req.model.API.effective() {
	case APIResponses:
		res, err = decodeStream(resp.Body, c.maxEvent, h.delta)
	case APIAnthropicMessages:
		res, err = decodeMessages(resp.Body, c.maxEvent, h.delta)
	default:
		err = errorf(CodeUnsupported, "unsupported model API %q", req.model.API)
	}
	if err != nil {
		var me *modelError
		errors.As(err, &me)
		if identifiableCancellation(ctx, err) {
			if me == nil || me.usage == nil {
				return nil, context.Cause(ctx)
			}
			me.cause, me.msg, me.retryable = context.Cause(ctx), context.Cause(ctx).Error(), false
		}
		if me != nil {
			if me.failure != nil {
				me.failure.HTTPStatus = resp.StatusCode
				me.failure.RequestID = resp.Header.Get("x-request-id")
				me.msg = providerFailureMessage(me.failure)
			}
			sanitizeModelError(me, ep.secrets...)
			if req.model.auth == AuthOpenAIChatGPT {
				return nil, c.auth.recordPlanFailure(req.model.account, me)
			}
			return nil, me
		}
		return nil, &modelError{msg: diagnostic("reading response stream: "+err.Error(), ep.secrets...), retryable: true}
	}
	authorizeOutput(res, req)
	return res, nil
}

// endpoint is the URL, headers, and credential of one request attempt.
type endpoint struct {
	url          string
	header       http.Header
	token        string   // bearer credential sent
	secrets      []string // redacted from diagnostics
	subscription bool
}

// endpoint resolves credentials for one attempt. API-key providers read
// their key now. Subscription providers obtain a valid credential of the
// operation's frozen account (refreshing it if needed) and add the
// provider's required headers. The Copilot origin is the configured
// base_url, or else the account's validated endpoint from login; the
// configured connection identity is never replaced by it.
func (c *modelClient) endpoint(ctx context.Context, req modelRequest, rejected string) (endpoint, error) {
	m := req.model
	ep, err := credentialTarget(ctx, Provider{BaseURL: m.BaseURL, Auth: m.auth, APIKey: m.apiKey, APIKeyEnv: m.apiKeyEnv}, c.auth, m.account, rejected)
	if err != nil {
		return endpoint{}, err
	}
	switch m.API.effective() {
	case APIResponses:
		ep.url += "/responses"
	case APIAnthropicMessages:
		ep.url += "/v1/messages?beta=true"
		ep.header.Set("anthropic-version", "2023-06-01")
		ep.secrets = append(ep.secrets, messagesSecrets(req.input)...)
	default:
		return endpoint{}, errorf(CodeUnsupported, "unsupported model API %q", m.API)
	}
	if m.auth == AuthGitHubCopilot {
		ep.header.Set("Openai-Intent", "conversation-edits")
		ep.header.Set("X-Initiator", cmp.Or(req.initiator, initiator(req.input)))
		if req.sessionID != "" {
			ep.header.Set("X-Client-Request-Id", req.sessionID)
		}
		if copilotVisionInput(req.input) {
			ep.header.Set("Copilot-Vision-Request", "true")
		}
	}
	return ep, nil
}

// initiator reports "user" when the request answers a user message and
// "agent" when it continues after the model's own output or tool results.
func initiator(input []json.RawMessage) string {
	if len(input) == 0 {
		return "user"
	}
	var it outputItem
	if json.Unmarshal(input[len(input)-1], &it) == nil && it.Type == "message" && it.Role == "user" {
		return "user"
	}
	return "agent"
}

// diagnostic redacts credentials first, then bounds the message, so a
// secret can never survive half-cut at the truncation point.
func diagnostic(s string, secrets ...string) string { return truncate(redact(s, secrets...), 1024) }

// parseRetryAfter accepts delay-seconds or an HTTP date. Invalid values return 0.
func parseRetryAfter(v string, now time.Time) time.Duration {
	delay, _ := parseRetryAdvice(v, now)
	return delay
}

func parseRetryAdvice(v string, now time.Time) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	if n, err := strconv.Atoi(v); err == nil {
		if n < 0 || uint64(n) > uint64((1<<63-1)/int64(time.Second)) {
			return 0, false
		}
		return time.Duration(n) * time.Second, true
	}
	t, err := http.ParseTime(v)
	if err != nil {
		return 0, false
	}
	return max(0, t.Sub(now)), true
}

// redact removes each credential, including a prefix of one left at the
// end of a body that was cut off by a read limit.
func redact(s string, secrets ...string) string {
	for _, key := range secrets {
		if key == "" {
			continue
		}
		s = strings.ReplaceAll(s, key, "[redacted]")
		if n := secretSuffix(s, key); n >= 4 {
			s = s[:len(s)-n] + "[redacted]"
		}
	}
	return s
}

// secretSuffix finds a cut-off secret prefix at the end of a captured field.
// Prefix matching is linear, including long/repetitive opaque payloads.
func secretSuffix(s, key string) int {
	n := min(len(s), len(key)-1)
	if n < 4 {
		return 0
	}
	prefix := key[:n]
	tail := s[len(s)-n:]
	if !strings.Contains(tail, prefix[:4]) {
		return 0
	}
	failure := make([]int, n)
	for i, q := 1, 0; i < n; i++ {
		for q > 0 && prefix[i] != prefix[q] {
			q = failure[q-1]
		}
		if prefix[i] == prefix[q] {
			q++
		}
		failure[i] = q
	}
	q := 0
	for i := range len(tail) {
		for q > 0 && (q == n || tail[i] != prefix[q]) {
			q = failure[q-1]
		}
		if tail[i] == prefix[q] {
			q++
		}
	}
	return q
}

// rejectsHistory recognizes a provider refusing saved opaque reasoning,
// for example encrypted content produced under another account or
// organization. Other request errors stay ordinary provider errors.
func rejectsHistory(code, msg string) bool {
	return code == "invalid_encrypted_content" || strings.Contains(strings.ToLower(msg), "encrypted content")
}

// reasoningPartSeparator joins reasoning summary parts, both while streaming
// and in saved history text.
const reasoningPartSeparator = "\n\n"

// streamEvent covers the Responses SSE events the client uses.
type streamEvent struct {
	Type        string `json:"type"`
	Delta       string `json:"delta"`
	OutputIndex int    `json:"output_index"`
	// SummaryIndex numbers the parts of one reasoning item's summary.
	SummaryIndex int             `json:"summary_index"`
	Item         json.RawMessage `json:"item"`
	Code         string          `json:"code"`
	Message      string          `json:"message"`
	Param        string          `json:"param"`
	Error        *wireFailure    `json:"error"`
	Response     *struct {
		Status string            `json:"status"`
		Output []json.RawMessage `json:"output"`
		Usage  *struct {
			InputTokens        *int64 `json:"input_tokens"`
			OutputTokens       *int64 `json:"output_tokens"`
			InputTokensDetails *struct {
				CachedTokens *int64 `json:"cached_tokens"`
			} `json:"input_tokens_details"`
			OutputTokensDetails *struct {
				ReasoningTokens *int64 `json:"reasoning_tokens"`
			} `json:"output_tokens_details"`
		} `json:"usage"`
		Error             *wireFailure `json:"error"`
		IncompleteDetails *struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
	} `json:"response"`
}

// decodeStream reads SSE frames until a terminal response event. EOF
// without a terminal event is an error, never success. One assembled event
// (all its data lines) may be at most maxEvent bytes.
func decodeStream(body io.Reader, maxEvent int, delta func(kind, text string)) (*modelResult, error) {
	br := bufio.NewReaderSize(body, 64<<10)
	items := map[int]json.RawMessage{}
	part := [2]int{-1, -1} // output and summary index of the last streamed summary part
	for {
		data, err := readProviderEvent(br, maxEvent)
		if err != nil {
			return nil, err
		}
		if string(data) == "[DONE]" {
			continue
		}
		var ev streamEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil, &modelError{msg: "malformed stream event: " + err.Error()}
		}
		if u := ev.usage(); u != nil {
			if err := u.validate(); err != nil {
				return nil, err
			}
		}
		// Summary parts are separate paragraphs; the saved item joins them the
		// same way (see histItem.visible), so live and saved text match.
		if ev.Type == "response.reasoning_summary_text.delta" && ev.Delta != "" {
			if next := [2]int{ev.OutputIndex, ev.SummaryIndex}; next != part {
				if part[0] >= 0 {
					delta("reasoning", reasoningPartSeparator)
				}
				part = next
			}
		}
		if res, err := handleEvent(ev, items, delta); res != nil || err != nil {
			var failure *modelError
			if errors.As(err, &failure) {
				failure.usage = ev.usage()
			}
			return res, err
		}
	}
}

func handleEvent(ev streamEvent, items map[int]json.RawMessage, delta func(kind, text string)) (*modelResult, error) {
	switch ev.Type {
	case "response.output_text.delta":
		delta("text", ev.Delta)
	case "response.refusal.delta":
		delta("refusal", ev.Delta)
	case "response.reasoning_summary_text.delta":
		delta("reasoning", ev.Delta)
	case "response.output_item.done":
		items[ev.OutputIndex] = ev.Item
	case "response.completed":
		return completed(ev, items)
	case "response.failed", "error":
		wire := wireFailure{Code: ev.Code, Message: ev.Message, Param: ev.Param}
		if ev.Error != nil {
			wire = *ev.Error
		}
		if ev.Response != nil && ev.Response.Error != nil {
			wire = *ev.Response.Error
		}
		return nil, providerModelError(&ProviderFailure{Code: wire.Code, Message: wire.Message, Param: wire.Param, Shape: "error"})
	case "response.incomplete":
		reason := "unknown"
		if ev.Response != nil && ev.Response.IncompleteDetails != nil {
			reason = ev.Response.IncompleteDetails.Reason
		}
		return nil, &modelError{msg: "response incomplete: " + reason}
	}
	return nil, nil
}

func (ev streamEvent) usage() *Usage {
	if ev.Response == nil || ev.Response.Usage == nil {
		return nil
	}
	u := ev.Response.Usage
	out := &Usage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens}
	if u.InputTokensDetails != nil {
		out.CacheReadTokens = u.InputTokensDetails.CachedTokens
	}
	if u.OutputTokensDetails != nil {
		out.ReasoningTokens = u.OutputTokensDetails.ReasoningTokens
	}
	return out
}

func completed(ev streamEvent, items map[int]json.RawMessage) (*modelResult, error) {
	if ev.Response == nil || ev.Response.Status != "completed" {
		return nil, &modelError{msg: "provider terminal response did not report completed status; output and tools were discarded"}
	}
	res := &modelResult{}
	if ev.Response != nil {
		res.output = ev.Response.Output
		if u := ev.usage(); u != nil {
			res.usage = *u
		}
	}
	if len(res.output) == 0 {
		for i := range len(items) {
			item, ok := items[i]
			if !ok {
				return nil, &modelError{msg: fmt.Sprintf("completed response is missing output item %d", i)}
			}
			res.output = append(res.output, item)
		}
	}
	seen := map[string]bool{}
	for _, item := range res.output {
		var it outputItem
		if err := json.Unmarshal(item, &it); err != nil || it.Type == "" {
			return nil, &modelError{msg: "completed response contains a malformed output item"}
		}
		if it.Type != "function_call" {
			continue
		}
		if it.CallID == "" || it.Name == "" || seen[it.CallID] || !json.Valid([]byte(it.Arguments)) {
			return nil, &modelError{msg: "completed response contains a malformed or duplicate function call"}
		}
		seen[it.CallID] = true
	}
	return res, nil
}

var errEventTooLarge = errors.New("provider stream event too large")

// readLine reads one line without its terminator ("\n" or "\r\n"). It
// fails with errEventTooLarge once the bytes it has buffered exceed room
// plus that terminator, before buffering more.
func readLine(br *bufio.Reader, room int) ([]byte, error) {
	room = max(room, 0)
	var line []byte
	for {
		chunk, err := br.ReadSlice('\n')
		if len(line)+len(chunk) > room+2 {
			return nil, errEventTooLarge
		}
		line = append(line, chunk...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil && (len(line) == 0 || !errors.Is(err, io.EOF)) {
			return nil, err
		}
		line = bytes.TrimSuffix(bytes.TrimSuffix(line, []byte("\n")), []byte("\r"))
		if len(line) > room {
			return nil, errEventTooLarge
		}
		return line, nil
	}
}

// outputItem is the subset of a Responses output item the runtime inspects.
type outputItem struct {
	Type      string `json:"type"`
	Role      string `json:"role"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
	Arguments string `json:"arguments"`
	Output    string `json:"output"`
	Content   []struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		Refusal string `json:"refusal"`
	} `json:"content"`
	Summary []struct {
		Text string `json:"text"`
	} `json:"summary"`
	EncryptedContent string          `json:"encrypted_content"`
	API              ModelAPI        `json:"api,omitempty"`
	Native           *messagesReplay `json:"native,omitempty"`
}

// text returns the item's visible text, including refusal text.
func (it outputItem) text() string {
	var sb strings.Builder
	for _, c := range it.Content {
		sb.WriteString(c.Text)
		sb.WriteString(c.Refusal)
	}
	return sb.String()
}

// refusal returns the refusal text of a message item, or "".
func (it outputItem) refusal() string {
	var sb strings.Builder
	for _, c := range it.Content {
		if c.Type == "refusal" {
			sb.WriteString(c.Refusal)
		}
	}
	return sb.String()
}
