package sam

import "slices"

// SessionOverrides records explicit choices. Nil fields inherit runtime defaults;
// pointers to empty strings, false, zero, and empty lists are explicit choices.
// Numeric zero selects the built-in numeric default, not the shared default.
// Replacing this value removes all omitted overrides.
type SessionOverrides struct {
	Model                  *string             `json:"model,omitempty"`
	ReasoningEffort        *string             `json:"reasoning_effort,omitempty"`
	ReasoningSummary       *bool               `json:"reasoning_summary,omitempty"`
	AppendInstructions     *string             `json:"append_instructions,omitempty"`
	AppendInstructionsFile *string             `json:"append_instructions_file,omitempty"`
	DisabledTools          *[]string           `json:"disabled_tools,omitempty"`
	ToolConcurrency        *int                `json:"tool_concurrency,omitempty"`
	ToolOutputLimit        *int                `json:"tool_output_limit,omitempty"`
	Account                *string             `json:"account,omitempty"`
	Compaction             CompactionOverrides `json:"compaction"`
}

// CompactionOverrides independently overrides each shared compaction setting.
type CompactionOverrides struct {
	Model          *string `json:"model,omitempty"`
	Account        *string `json:"account,omitempty"`
	Prompt         *string `json:"prompt,omitempty"`
	PromptFile     *string `json:"prompt_file,omitempty"`
	DisableAuto    *bool   `json:"disable_auto,omitempty"`
	OutputReserve  *int    `json:"output_reserve,omitempty"`
	TriggerPercent *int    `json:"trigger_percent,omitempty"`
	RecentPercent  *int    `json:"recent_percent,omitempty"`
}

// SessionSettings separates installed effective settings from explicit intent.
// Edit Overrides and pass it to UpdateConfig; do not turn Effective into overrides
// unless you deliberately want to pin every setting.
type SessionSettings struct {
	Resources PreparedResources `json:"resources"`
	Effective SessionConfig     `json:"effective"`
	Overrides SessionOverrides  `json:"overrides"`
}

func (o SessionOverrides) snapshot() SessionOverrides {
	o.Model = copyChoice(o.Model)
	o.ReasoningEffort = copyChoice(o.ReasoningEffort)
	o.ReasoningSummary = copyChoice(o.ReasoningSummary)
	o.AppendInstructions = copyChoice(o.AppendInstructions)
	o.AppendInstructionsFile = copyChoice(o.AppendInstructionsFile)
	o.ToolConcurrency = copyChoice(o.ToolConcurrency)
	o.ToolOutputLimit = copyChoice(o.ToolOutputLimit)
	o.Account = copyChoice(o.Account)
	// A pointer to a nil list is an explicit empty list, not JSON null/inheritance.
	if o.DisabledTools != nil {
		o.DisabledTools = new(append([]string{}, (*o.DisabledTools)...))
	}
	o.Compaction = o.Compaction.snapshot()
	return o
}

func (o CompactionOverrides) snapshot() CompactionOverrides {
	o.Model = copyChoice(o.Model)
	o.Account = copyChoice(o.Account)
	o.Prompt = copyChoice(o.Prompt)
	o.PromptFile = copyChoice(o.PromptFile)
	o.DisableAuto = copyChoice(o.DisableAuto)
	o.OutputReserve = copyChoice(o.OutputReserve)
	o.TriggerPercent = copyChoice(o.TriggerPercent)
	o.RecentPercent = copyChoice(o.RecentPercent)
	return o
}

func copyChoice[T any](p *T) *T {
	if p == nil {
		return nil
	}
	return new(*p)
}

func chosen[T any](fallback T, override *T) T {
	if override != nil {
		return *override
	}
	return fallback
}

func (o SessionOverrides) apply(c SessionConfig) SessionConfig {
	c.Model = chosen(c.Model, o.Model)
	c.ReasoningEffort = chosen(c.ReasoningEffort, o.ReasoningEffort)
	c.ReasoningSummary = chosen(c.ReasoningSummary, o.ReasoningSummary)
	c.AppendInstructions = chosen(c.AppendInstructions, o.AppendInstructions)
	c.AppendInstructionsFile = chosen(c.AppendInstructionsFile, o.AppendInstructionsFile)
	c.DisabledTools = slices.Clone(chosen(c.DisabledTools, o.DisabledTools))
	c.ToolConcurrency = chosen(c.ToolConcurrency, o.ToolConcurrency)
	c.ToolOutputLimit = chosen(c.ToolOutputLimit, o.ToolOutputLimit)
	c.Account = chosen(c.Account, o.Account)
	c.Compaction = o.Compaction.apply(c.Compaction)
	return c
}

func (o CompactionOverrides) apply(c CompactionConfig) CompactionConfig {
	c.Model = chosen(c.Model, o.Model)
	c.Account = chosen(c.Account, o.Account)
	c.Prompt = chosen(c.Prompt, o.Prompt)
	c.PromptFile = chosen(c.PromptFile, o.PromptFile)
	c.DisableAuto = chosen(c.DisableAuto, o.DisableAuto)
	c.OutputReserve = chosen(c.OutputReserve, o.OutputReserve)
	c.TriggerPercent = chosen(c.TriggerPercent, o.TriggerPercent)
	c.RecentPercent = chosen(c.RecentPercent, o.RecentPercent)
	return c
}

// explicitConfig conservatively migrates legacy full settings. Missing JSON
// fields used to mean zero, not inheritance, so those choices are pinned too.
func explicitConfig(c SessionConfig) SessionOverrides {
	if c.DisabledTools == nil {
		c.DisabledTools = []string{}
	}
	return SessionOverrides{
		Model: new(c.Model), ReasoningEffort: new(c.ReasoningEffort), ReasoningSummary: new(c.ReasoningSummary),
		AppendInstructions: new(c.AppendInstructions), AppendInstructionsFile: new(c.AppendInstructionsFile),
		DisabledTools: new(slices.Clone(c.DisabledTools)), ToolConcurrency: new(c.ToolConcurrency),
		ToolOutputLimit: new(c.ToolOutputLimit), Account: new(c.Account),
		Compaction: CompactionOverrides{
			Model: new(c.Compaction.Model), Account: new(c.Compaction.Account), Prompt: new(c.Compaction.Prompt),
			PromptFile: new(c.Compaction.PromptFile), DisableAuto: new(c.Compaction.DisableAuto),
			OutputReserve: new(c.Compaction.OutputReserve), TriggerPercent: new(c.Compaction.TriggerPercent),
			RecentPercent: new(c.Compaction.RecentPercent),
		},
	}
}
