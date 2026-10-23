package sam

import (
	"testing"
)

func TestCatalogUnknownNoneAndReferences(t *testing.T) {
	raw := []byte(`{"data":[{"id":"gpt-6-sol","supported_endpoints":[],"capabilities":{"supports":{"thinking":false,"reasoning_effort":[]}}},{"id":"future-model","capabilities":{"limits":{"max_prompt_tokens":9999},"supports":{"thinking":true}}}]}`)
	models, err := parseCatalog(raw, AuthGitHubCopilot)
	if err != nil {
		t.Fatal(err)
	}
	first, unknown := models[0], models[1]
	if first.ContextTokens != nil || first.ReasoningEfforts == nil || len(first.ReasoningEfforts) != 0 || first.SupportsResponses == nil || *first.SupportsResponses || first.SupportsReasoning == nil || *first.SupportsReasoning {
		t.Fatalf("false/empty facts replaced with reference: %+v", first)
	}
	if first.Reference == nil || unknown.Reference != nil || unknown.ContextTokens != nil || unknown.ReasoningEfforts != nil || unknown.SupportsResponses != nil {
		t.Fatalf("invented facts: %+v %+v", first, unknown)
	}
	first.Reference.ContextTokens = 1
	first.Reference.Source = "mutated"
	again, err := parseCatalog(raw, AuthGitHubCopilot)
	if err != nil || again[0].Reference.ContextTokens != 1000000 || again[0].Reference.Source == "mutated" {
		t.Fatal("reference shared across calls", again, err)
	}
	keyModels, err := parseCatalog([]byte(`{"data":[{"id":"gpt-6-sol"}]}`), "")
	if err != nil || keyModels[0].Reference != nil {
		t.Fatal("reference leaked to an arbitrary API-key endpoint", keyModels, err)
	}
	chat, err := parseCatalog([]byte(`{"models":[{"slug":"gpt-6-sol","visibility":"list"},{"slug":"hidden","visibility":"hide"}]}`), AuthOpenAIChatGPT)
	if err != nil || len(chat) != 1 || chat[0].ContextTokens != nil || chat[0].ReasoningEfforts != nil || chat[0].Reference.ContextTokens != 272000 {
		t.Fatal("ChatGPT reference or filtering", chat, err)
	}
}

func TestMalformedCatalogMetadata(t *testing.T) {
	for _, fields := range []string{
		`"context_window":0`, `"context_window":-1`, `"context_window":1.5`,
		`"supported_reasoning_levels":[{"effort":""}]`,
		`"supported_reasoning_levels":[{"effort":"high"},{"effort":"high"}]`,
		`"supported_reasoning_levels":[{"effort":"high\n"}]`,
		`"supports_reasoning_summary_parameter":"yes"`,
	} {
		if _, err := parseCatalog([]byte(`{"models":[{"slug":"x","visibility":"list",`+fields+`}]}`), AuthOpenAIChatGPT); err == nil {
			t.Errorf("accepted %s", fields)
		}
	}
	models, err := parseCatalog([]byte(`{"models":[{"slug":"x","visibility":"list","context_window":null,"supported_reasoning_levels":[],"supports_reasoning_summaries":true}]}`), AuthOpenAIChatGPT)
	if err != nil || models[0].ContextTokens != nil || models[0].ReasoningEfforts == nil || models[0].ReasoningSummaries == nil || !*models[0].ReasoningSummaries {
		t.Fatal(models, err)
	}
}

func TestCopilotMessagesMetadataKeepsProviderFacts(t *testing.T) {
	raw := []byte(`{"data":[{"id":"claude-opus-5.5","policy":{"state":"enabled"},"supported_endpoints":["/v1/messages"],"capabilities":{"limits":{"max_context_window_tokens":1000000,"max_output_tokens":64000},"supports":{"thinking":true,"reasoning_effort":["low","high","xhigh","max"]}}},{"id":"claude-haiku-4.5","supported_endpoints":["/v1/messages"]},{"id":"future"},{"id":"dual","supported_endpoints":["/responses","/v1/messages"]}]}`)
	models, err := parseCatalog(raw, AuthGitHubCopilot)
	if err != nil {
		t.Fatal(err)
	}
	m := models[0]
	if m.API != APIAnthropicMessages || m.SupportsResponses == nil || *m.SupportsResponses || m.SupportsMessages == nil || !*m.SupportsMessages || m.MaxOutputTokens == nil || *m.MaxOutputTokens != 64000 || m.ReasoningSummaries != nil {
		t.Fatal(m)
	}
	if models[1].API != "" || models[1].MaxOutputTokens != nil || models[2].SupportsMessages != nil || models[2].SupportsResponses != nil || models[3].API != APIResponses {
		t.Fatal(models)
	}
	contradiction, err := parseCatalog([]byte(`{"data":[{"id":"claude-opus-5.5","supported_endpoints":["/v1/messages"],"capabilities":{"supports":{"thinking":false}}}]}`), AuthGitHubCopilot)
	if err != nil || contradiction[0].API != "" || contradiction[0].SupportsReasoning == nil || *contradiction[0].SupportsReasoning {
		t.Fatal("reference overrode explicit false thinking", contradiction, err)
	}
	for _, output := range []string{"0", "-1", `"64000"`} {
		if _, err := parseCatalog([]byte(`{"data":[{"id":"bad","capabilities":{"limits":{"max_output_tokens":`+output+`}}}]}`), AuthGitHubCopilot); err == nil {
			t.Fatal("invalid output limit accepted", output)
		}
	}
}
