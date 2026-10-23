package sam

import (
	"encoding/json"
	"net/http"
	"time"
)

// DefaultGitHubCopilotClientID is the public GitHub-owned Copilot Plugin app ID
// used by Pi. It is application identification, not a credential or a sam-g
// registration. Users still authorize their own account through device login.
const DefaultGitHubCopilotClientID = "Iv1.b507a08c87ecfe98"

// Compatibility profile pinned to @earendil-works/pi-ai 0.87.1. Keep auth,
// catalog and inference identification together; don't drift between clients.
// This is an observed integration profile, not a guarantee of GitHub access.
const (
	copilotUserAgent    = "GitHubCopilotChat/0.35.0"
	copilotEditor       = "vscode/1.107.0"
	copilotPlugin       = "copilot-chat/0.35.0"
	copilotIntegration  = "vscode-chat"
	copilotAPIVersion   = "2026-06-01"
	copilotDefaultAPI   = "https://api.individual.githubcopilot.com"
	copilotRefreshAhead = 5 * time.Minute
)

func copilotHeaders() http.Header {
	h := http.Header{}
	h.Set("User-Agent", copilotUserAgent)
	h.Set("Editor-Version", copilotEditor)
	h.Set("Editor-Plugin-Version", copilotPlugin)
	h.Set("Copilot-Integration-Id", copilotIntegration)
	return h
}

func copilotDeviceHeaders() http.Header {
	h := http.Header{}
	h.Set("User-Agent", copilotUserAgent)
	return h
}

func copilotVisionInput(input []json.RawMessage) bool {
	type part struct {
		Type string `json:"type"`
	}
	for _, raw := range input {
		var item struct {
			Type    string          `json:"type"`
			Role    string          `json:"role"`
			Content []part          `json:"content"`
			Output  json.RawMessage `json:"output"`
		}
		if json.Unmarshal(raw, &item) != nil {
			continue
		}
		parts := item.Content
		if item.Type == "function_call_output" {
			_ = json.Unmarshal(item.Output, &parts)
		} else if item.Role != "user" {
			continue
		}
		for _, p := range parts {
			if p.Type == "input_image" {
				return true
			}
		}
	}
	return false
}
