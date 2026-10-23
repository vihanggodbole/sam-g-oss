package sam

import (
	"context"
	"encoding/json"
	"fmt"
)

func subagentsTool(r *Runtime) Tool {
	return Tool{
		Name:        "subagents",
		Description: "Start, inspect, steer or cancel one-level child sessions. List installed models to choose a provider and thinking effort; read child output with history.",
		Parameters: json.RawMessage(`{"type":"object","properties":{
   "action":{"type":"string","enum":["models","start","status","steer","cancel"]},
   "subagent_id":{"type":"string","description":"Owned child ID; required for steer/cancel, optional for status (omitted lists children)."},
   "name":{"type":"string","description":"start: short task label"},
   "prompt":{"type":"string","description":"start: task instructions"},
   "workspace":{"type":"string","description":"start: existing absolute directory; omitted inherits parent. Shared directories are allowed; no edit coordination."},
   "config":{"type":"object","description":"start: session overrides. Model alias determines provider; a different model requires explicit account and defaults reasoning/reserve before overrides. Omitted fields inherit. Disabled tools can only be added; children cannot delegate.","properties":{
     "model":{"type":"string"},"account":{"type":"string","description":"Named account, or empty for API-key models."},
     "reasoning_effort":{"type":"string","description":"Thinking effort declared by the selected model, e.g. medium; empty uses provider default."},
     "reasoning_summary":{"type":"boolean"},
     "append_instructions":{"type":"string"},"append_instructions_file":{"type":"string"},
     "disabled_tools":{"type":"array","items":{"type":"string"}},
     "tool_concurrency":{"type":"integer","minimum":0},"tool_output_limit":{"type":"integer","minimum":0},
     "compaction":{"type":"object","properties":{
       "model":{"type":"string"},"account":{"type":"string"},"prompt":{"type":"string"},"prompt_file":{"type":"string"},
       "disable_auto":{"type":"boolean"},"output_reserve":{"type":"integer","minimum":0},
       "trigger_percent":{"type":"integer","minimum":0,"maximum":100},"recent_percent":{"type":"integer","minimum":0,"maximum":100}
     },"additionalProperties":false}
   },"additionalProperties":false},
   "text":{"type":"string","description":"steer: guidance for the next model request; does not rewrite an in-flight call."}
  },"required":["action"],"additionalProperties":false}`),
		Run: r.runSubagentsTool,
	}
}

type subagentArgs struct {
	Action     string            `json:"action"`
	SubagentID string            `json:"subagent_id"`
	Name       string            `json:"name"`
	Prompt     string            `json:"prompt"`
	Workspace  string            `json:"workspace"`
	Config     *SessionOverrides `json:"config"`
	Text       string            `json:"text"`
}

func (a subagentArgs) check() error {
	start := a.Name != "" || a.Prompt != "" || a.Workspace != "" || a.Config != nil
	if a.Action != "start" && start {
		return fmt.Errorf("name, prompt, workspace and config are only used with start")
	}
	if a.Action != "steer" && a.Text != "" {
		return fmt.Errorf("text is only used with steer")
	}
	switch a.Action {
	case "start", "models":
		if a.SubagentID != "" {
			return fmt.Errorf("%s does not accept subagent_id", a.Action)
		}
	case "status":
	case "steer", "cancel":
		if a.SubagentID == "" {
			return fmt.Errorf("%s requires subagent_id", a.Action)
		}
	default:
		return fmt.Errorf("action must be models, start, status, steer or cancel")
	}
	return nil
}

func (r *Runtime) runSubagentsTool(ctx context.Context, c ToolCall) (string, error) {
	var a subagentArgs
	if err := decodeArgs(c.Arguments, &a); err != nil {
		return "", err
	}
	if err := a.check(); err != nil {
		return "", err
	}
	caller, err := loadSession(r.reader(ctx), c.SessionID)
	if err != nil {
		return "", err
	}
	if caller.isSubagent {
		return "", errorf(CodeUnsupported, "child sessions cannot use the subagents tool")
	}
	// Attempts are excluded: replay rejoins the same accepted child.
	key := "subagents/" + c.SessionID + "/" + c.TurnID + "/" + c.CallID
	if a.SubagentID != "" {
		if err := ownedSubagent(r.reader(ctx), c.SessionID, a.SubagentID); err != nil {
			return "", err
		}
	}
	var out any
	switch a.Action {
	case "models":
		out = r.subagentModels()
	case "start":
		out, err = r.StartSubagent(ctx, StartSubagentRequest{RequestID: key, ParentID: c.SessionID, Name: a.Name, Prompt: a.Prompt, Workspace: a.Workspace, Config: a.Config})
	case "status":
		if a.SubagentID == "" {
			out, err = r.Subagents(ctx, c.SessionID)
		} else {
			r.mu.Lock()
			out, err = r.subagentView(r.reader(ctx), a.SubagentID)
			r.mu.Unlock()
		}
	case "steer":
		out, err = r.Steer(ctx, SteerRequest{RequestID: key, SessionID: a.SubagentID, Text: a.Text})
	case "cancel":
		err = r.Interrupt(ctx, SessionRequest{RequestID: key, SessionID: a.SubagentID})
		out = map[string]string{"session_id": a.SubagentID, "state": "cancellation requested; inspect status for settlement"}
	}
	if err != nil {
		return "", err
	}
	return mustJSON(out), nil
}

func (r *Runtime) subagentModels() any {
	accounts := []map[string]string{}
	if auth := r.Auth(); auth != nil {
		for _, a := range auth.Accounts() {
			accounts = append(accounts, map[string]string{"name": a.Name, "provider": a.Provider, "status": a.Status})
		}
	}
	return map[string]any{"models": r.Models(), "providers": r.Providers(), "accounts": accounts}
}
