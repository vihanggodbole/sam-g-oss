package sam

import (
	"encoding/json"
	"slices"
	"strings"
	"unicode"
)

// catalogCapabilities only interprets the provider-specific HTTP catalog
// fields cited in docs/MODEL-METADATA.md. In particular, prompt limits are not
// context windows, and a thinking boolean is not a list of reasoning efforts.
func catalogCapabilities(raw json.RawMessage, auth string, m *DiscoveredModel) error {
	var err error
	switch auth {
	case AuthOpenAIChatGPT:
		err = chatGPTCapabilities(raw, m)
	case AuthGitHubCopilot:
		err = copilotCapabilities(raw, m)
	}
	if err != nil || m.ContextTokens != nil && *m.ContextTokens <= 0 || m.MaxOutputTokens != nil && *m.MaxOutputTokens <= 0 {
		return errorf(CodeProviderError, "invalid model catalog capability metadata")
	}
	seen := map[string]bool{}
	for _, effort := range m.ReasoningEfforts {
		if strings.TrimSpace(effort) != effort || effort == "" || strings.ContainsFunc(effort, unicode.IsControl) || seen[effort] {
			return errorf(CodeProviderError, "invalid model catalog reasoning efforts")
		}
		seen[effort] = true
	}
	return nil
}

func chatGPTCapabilities(raw json.RawMessage, m *DiscoveredModel) error {
	var v struct {
		ContextWindow *int `json:"context_window"`
		Levels        []struct {
			Effort string `json:"effort"`
		} `json:"supported_reasoning_levels"`
		Summaries        *bool `json:"supports_reasoning_summaries"`
		SummaryParameter *bool `json:"supports_reasoning_summary_parameter"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	m.ContextTokens = v.ContextWindow
	m.ReasoningSummaries = v.SummaryParameter
	if m.ReasoningSummaries == nil {
		m.ReasoningSummaries = v.Summaries
	}
	if v.Levels != nil {
		m.ReasoningEfforts = make([]string, 0, len(v.Levels))
		for _, level := range v.Levels {
			m.ReasoningEfforts = append(m.ReasoningEfforts, level.Effort)
		}
	}
	return nil
}

func copilotCapabilities(raw json.RawMessage, m *DiscoveredModel) error {
	var v struct {
		Endpoints    []string `json:"supported_endpoints"`
		Capabilities struct {
			Limits struct {
				Context *int `json:"max_context_window_tokens"`
				Output  *int `json:"max_output_tokens"`
			} `json:"limits"`
			Supports struct {
				Thinking *bool    `json:"thinking"`
				Efforts  []string `json:"reasoning_effort"`
				Vision   *bool    `json:"vision"`
			} `json:"supports"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	m.ContextTokens = v.Capabilities.Limits.Context
	m.MaxOutputTokens = v.Capabilities.Limits.Output
	m.SupportsReasoning = v.Capabilities.Supports.Thinking
	m.ReasoningEfforts = v.Capabilities.Supports.Efforts
	m.SupportsImages = v.Capabilities.Supports.Vision
	if v.Endpoints != nil {
		m.SupportsResponses = new(slices.Contains(v.Endpoints, "/responses"))
		m.SupportsMessages = new(slices.Contains(v.Endpoints, "/v1/messages"))
	}
	return nil
}
