package sam

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"unicode/utf8"
)

// Hook is a host-owned typed registration, not a middleware chain. IDs must
// be nonblank and unique within a stage. Handle must cooperate with ctx.
// Callbacks run outside runtime locks and may use read APIs, but must not
// wait for their own operation to settle or synchronously Close/Reload.
// Functions and captured mutable state remain the host's responsibility.
type Hook[I, O any] struct {
	ID     string
	Handle func(context.Context, I) (O, error)
}

// Hooks registers independent handlers. A stage invokes them serially with
// detached copies of the same input, without a promised invocation order.
// Different sessions and tool calls may invoke them concurrently. Reload
// replaces registrations only when every session is fully idle.
type Hooks struct {
	BeforeTurn       []Hook[BeforeTurnInput, string]
	BeforeRequest    []Hook[BeforeRequestInput, string]
	BeforeTool       []Hook[BeforeToolInput, HookDecision]
	BeforeCompaction []Hook[BeforeCompactionInput, CompactionDecision]
}

// HookScope identifies an operation without credentials. Step is one-based
// for conversational requests/tools, zero before a turn or manual compaction.
// OperationID is the turn ID, or the compaction ID for BeforeCompaction.
type HookScope struct {
	SessionID, TurnID, OperationID string
	Step, Attempt                  int
	Workspace, Model, Account      string
}

// BeforeTurnInput contains this new turn's genuine accepted messages, not
// the whole transcript. Instructions is the installed base, never hook text.
type BeforeTurnInput struct {
	Scope        HookScope
	Source       string // prompt, continue, steering, follow_up
	Instructions string
	UserMessages []HistoryItem
}

// BeforeRequestInput describes the committed view before this invocation.
// Pending BeforeTurn additions are private candidates, not committed history.
// Use History for paginated reads; HistoryThrough is the snapshot high-water.
type BeforeRequestInput struct {
	Scope                 HookScope
	Instructions, Summary string
	HistoryThrough        int64
	Tools                 []ToolInfo
}

// BeforeToolInput describes an original planned call. Arguments retains the
// exact inspected bytes; valid non-object JSON remains valid for custom tools.
type BeforeToolInput struct {
	Scope     HookScope
	CallID    string
	Tool      ToolInfo
	Arguments json.RawMessage
}

// BeforeCompactionInput describes the frozen summarization selection. Hooks
// cannot change the cut, model, account, algorithm or replace the summary.
type BeforeCompactionInput struct {
	Scope                                 HookScope
	Automatic                             bool
	MainModel, Prompt, PreviousSummary    string
	FromSeq, ThroughSeq, ProtectedUserSeq int64
	EstimatedInputTokens, InputBudget     int
}

// HookAction is an explicit allow or deny. The zero value is invalid.
type HookAction string

const (
	HookAllow HookAction = "allow"
	HookDeny  HookAction = "deny"
)

// HookDecision requires an empty Reason for allow, nonblank for deny.
// Reasons and callback errors must be safe for public history/events.
type HookDecision struct {
	Action HookAction
	Reason string
}

// CompactionDecision may append trusted, operation-scoped summarization
// instructions on allow only. Accepted text stays in conversation history.
type CompactionDecision struct {
	Action             HookAction
	Reason             string
	AppendInstructions string
}

// ExtensionSource identifies a retained application contribution, not a human
// message. The wire role and labelled body are saved once alongside it.
type ExtensionSource struct {
	Stage       string `json:"stage"`
	HandlerID   string `json:"handler_id"`
	OperationID string `json:"operation_id"`
	Step        int    `json:"step"`
	Attempt     int    `json:"attempt,omitempty"`
}

const (
	stageTurn       = "before_turn"
	stageRequest    = "before_request"
	stageTool       = "before_tool"
	stageCompaction = "before_compaction"
	kindExtension   = "extension"
	// EventHookResult records decisions, direct failures and accepted history
	// references. It contains no contributed text or callback timing trace.
	EventHookResult = "hook.result"
	// CodeHookFailed means a callback failed or returned an invalid value.
	CodeHookFailed ErrorCode = "hook_failed"
	// CodeToolDenied is a normal tool-policy refusal, delivered as a tool result.
	CodeToolDenied ErrorCode = "tool_denied"
	// CodeCompactionDenied stops/pauses the summarization operation.
	CodeCompactionDenied ErrorCode = "compaction_denied"
)

// HookResultPayload is the small hook.result diagnostic. StopHandler identifies
// only a direct callback error/invalid return/denial, never a mixed-input fit
// rejection. Accepted contributions reference History rather than copy text.
type HookResultPayload struct {
	Stage              string                `json:"stage"`
	Step               int                   `json:"step"`
	Outcome            string                `json:"outcome"`
	Handlers           []HookHandlerDecision `json:"handlers,omitempty"`
	Contributions      []HookContribution    `json:"contributions,omitempty"`
	BudgetContributors []HookContributor     `json:"budget_contributors,omitempty"`
	StopHandler        string                `json:"stop_handler,omitempty"`
	Code               ErrorCode             `json:"code,omitempty"`
}

// HookHandlerDecision is a reached, valid decision (not proof of execution).
type HookHandlerDecision struct {
	ID     string     `json:"id"`
	Action HookAction `json:"action"`
}

// HookContribution references one accepted extension history entry.
type HookContribution struct {
	ID         string `json:"id"`
	HistorySeq int64  `json:"history_seq"`
}

// HookContributor identifies a candidate in a rejected combined input.
type HookContributor struct {
	Stage string `json:"stage"`
	ID    string `json:"id"`
}

func (h Hooks) clone() Hooks {
	h.BeforeTurn = slices.Clone(h.BeforeTurn)
	h.BeforeRequest = slices.Clone(h.BeforeRequest)
	h.BeforeTool = slices.Clone(h.BeforeTool)
	h.BeforeCompaction = slices.Clone(h.BeforeCompaction)
	return h
}

func (h Hooks) validate() error {
	for _, err := range []error{
		validateHooks(stageTurn, h.BeforeTurn), validateHooks(stageRequest, h.BeforeRequest),
		validateHooks(stageTool, h.BeforeTool), validateHooks(stageCompaction, h.BeforeCompaction),
	} {
		if err != nil {
			return err
		}
	}
	return nil
}

func validateHooks[I, O any](stage string, hooks []Hook[I, O]) error {
	ids := map[string]bool{}
	for _, h := range hooks {
		if strings.TrimSpace(h.ID) == "" || !utf8.ValidString(h.ID) || h.Handle == nil || ids[h.ID] {
			return errorf(CodeInvalidArgument, "%s: hook IDs must be nonblank valid UTF-8 and unique within the stage, with a nonnil Handle", stage)
		}
		ids[h.ID] = true
	}
	return nil
}

func validDecision(action HookAction, reason, text string) bool {
	if !utf8.ValidString(reason) || !utf8.ValidString(text) {
		return false
	}
	switch action {
	case HookAllow:
		return reason == ""
	case HookDeny:
		return strings.TrimSpace(reason) != "" && text == ""
	}
	return false
}

func (t *turnRun) hookScope() HookScope {
	return HookScope{
		SessionID: t.sessionID, TurnID: t.turnID, OperationID: t.turnID,
		Step: t.step, Attempt: t.attempt, Workspace: t.workspace, Model: t.model.Alias, Account: t.model.accountName(),
	}
}
