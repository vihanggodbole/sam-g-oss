package sam

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"
)

// DiscoverModelsRequest is Go-only wiring, not a remotely accepted body.
// Never serialize/log it: Provider can contain private configuration.
type DiscoverModelsRequest struct {
	Provider   Provider
	Account    string
	Auth       *AuthManager
	HTTPClient *http.Client
}

// DiscoveredModel reports upstream facts, not installed runtime configuration.
// Nil capabilities are unknown. ReasoningEfforts nil is unknown; [] means none.
// Reference, when present, is separately sourced advice, not an upstream fact.
type DiscoveredModel struct {
	ID                 string          `json:"id"`
	DisplayName        string          `json:"display_name"`
	Availability       string          `json:"availability"`
	API                ModelAPI        `json:"api,omitempty"` // selected supported codec, not an inferred provider fact
	SupportsMessages   *bool           `json:"supports_messages,omitempty"`
	MaxOutputTokens    *int            `json:"max_output_tokens,omitempty"`
	SupportsResponses  *bool           `json:"supports_responses,omitempty"`
	ContextTokens      *int            `json:"context_tokens,omitempty"`
	ReasoningEfforts   []string        `json:"reasoning_efforts"`
	ReasoningSummaries *bool           `json:"reasoning_summaries,omitempty"`
	SupportsReasoning  *bool           `json:"supports_reasoning,omitempty"`
	Reference          *ModelReference `json:"reference,omitempty"`
}

const maxCatalogBody = 32 << 20

// DiscoverModels makes one explicit catalog GET after normal on-demand auth
// resolution. No runtime, conversation database, catalog persistence, cache,
// retry, inference, enablement or model installation is involved.
func DiscoverModels(ctx context.Context, req DiscoverModelsRequest) ([]DiscoveredModel, error) {
	p, err := normalizeProvider(req.Provider)
	if err != nil {
		return nil, err
	}
	var ref accountRef
	if p.Auth == "" {
		if req.Account != "" {
			return nil, errorf(CodeInvalidArgument, "Account must be empty for API-key discovery")
		}
	} else {
		if req.Account == "" || req.Auth == nil {
			return nil, errorf(CodeInvalidArgument, "subscription discovery requires an explicit saved account and AuthManager")
		}
		ref, err = req.Auth.ref(p.Auth, req.Account)
		if err != nil {
			return nil, err
		}
	}
	ep, err := credentialTarget(ctx, p, req.Auth, ref, "")
	if err != nil {
		return nil, publicProviderError(err)
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodGet, ep.url+"/models", nil)
	if err != nil {
		return nil, errorf(CodeInvalidArgument, "cannot construct model catalog request")
	}
	hr.Header = ep.header
	hr.Header.Set("Accept", jsonType)
	if p.Auth == AuthGitHubCopilot {
		hr.Header.Set("X-GitHub-Api-Version", copilotAPIVersion)
	}
	// Refuse redirects even for API keys: discovery never forwards secrets to a
	// different catalog endpoint or silently substitutes another route.
	resp, err := noRedirectClient(req.HTTPClient).Do(hr)
	if err != nil {
		if identifiableCancellation(ctx, err) {
			return nil, context.Cause(ctx)
		}
		return nil, &Error{Code: CodeProviderError, Message: "model catalog transport failed", ProviderFailure: &ProviderFailure{Message: "model catalog transport failed", Shape: "unknown", Retryable: true}}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var failure error = statusError(resp, ep.secrets...)
		if p.Auth == AuthOpenAIChatGPT {
			failure = req.Auth.recordPlanFailure(ref, failure)
		}
		return nil, publicProviderError(failure)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxCatalogBody+1))
	if len(raw) > maxCatalogBody {
		return nil, errorf(CodeProviderError, "model catalog exceeds the 32 MiB limit; request a smaller supported catalog from the provider")
	}
	if err != nil {
		if identifiableCancellation(ctx, err) {
			return nil, context.Cause(ctx)
		}
		return nil, errorf(CodeProviderError, "reading model catalog failed")
	}
	return parseCatalog(raw, p.Auth)
}

// DiscoverModels snapshots host wiring and releases the runtime lock before I/O.
// It does not change configured aliases or the selected/bound conversation.
func (r *Runtime) DiscoverModels(ctx context.Context, provider, account string) ([]DiscoveredModel, error) {
	r.mu.Lock()
	p, ok := r.cfg.Providers[provider]
	req := DiscoverModelsRequest{Provider: p, Account: account, Auth: r.cfg.Auth, HTTPClient: r.cfg.HTTPClient}
	r.mu.Unlock()
	if !ok {
		return nil, errorf(CodeInvalidArgument, "provider alias %q is not configured", provider)
	}
	return DiscoverModels(ctx, req)
}

func parseCatalog(raw []byte, auth string) ([]DiscoveredModel, error) {
	var envelope struct {
		Models   json.RawMessage `json:"models"`
		Data     json.RawMessage `json:"data"`
		HasMore  bool            `json:"has_more"`
		Next     json.RawMessage `json:"next"`
		NextPage json.RawMessage `json:"next_page"`
	}
	bad := errorf(CodeProviderError, "provider returned a malformed or unsupported model catalog envelope")
	if !utf8.Valid(raw) || json.Unmarshal(raw, &envelope) != nil || envelope.HasMore || hasPage(envelope.Next) || hasPage(envelope.NextPage) {
		return nil, bad
	}
	items := envelope.Data
	if auth == AuthOpenAIChatGPT {
		items = envelope.Models
	}
	if len(items) == 0 || string(items) == "null" {
		return nil, bad
	}
	var entries []json.RawMessage
	if json.Unmarshal(items, &entries) != nil {
		return nil, bad
	}
	out := []DiscoveredModel{}
	seen := map[string]bool{}
	for _, raw := range entries {
		model, include, err := catalogEntry(raw, auth)
		if err != nil {
			return nil, err
		}
		if !include {
			continue
		}
		if strings.TrimSpace(model.ID) == "" || !utf8.ValidString(model.ID) || strings.ContainsFunc(model.ID, unicode.IsControl) || seen[model.ID] {
			return nil, errorf(CodeProviderError, "catalog has a blank, invalid or duplicate model ID")
		}
		seen[model.ID] = true
		if model.DisplayName == "" {
			model.DisplayName = model.ID
		}
		out = append(out, model)
	}
	return out, nil
}

func hasPage(raw json.RawMessage) bool {
	return len(raw) > 0 && string(raw) != "null" && string(raw) != "\"\""
}

func catalogEntry(raw json.RawMessage, auth string) (DiscoveredModel, bool, error) {
	var v struct {
		ID          string `json:"id"`
		Slug        string `json:"slug"`
		Name        string `json:"name"`
		DisplayName string `json:"display_name"`
		Visibility  string `json:"visibility"`
		Policy      *struct {
			State string `json:"state"`
		} `json:"policy"`
	}
	if json.Unmarshal(raw, &v) != nil {
		return DiscoveredModel{}, false, errorf(CodeProviderError, "malformed model catalog entry")
	}
	m := DiscoveredModel{ID: v.ID, DisplayName: v.Name, Availability: "unknown"}
	switch auth {
	case AuthOpenAIChatGPT:
		if v.Visibility != "list" {
			return m, false, nil
		}
		m.ID = v.Slug
		m.DisplayName = v.DisplayName
		m.Availability = "available"
		m.SupportsResponses = new(true)
	case AuthGitHubCopilot:
		if v.Policy != nil {
			switch v.Policy.State {
			case "enabled":
				m.Availability = "available"
			case "disabled":
				m.Availability = "unavailable"
			case "unconfigured":
				m.Availability = "requires_action"
			}
		}
	default:
		m.DisplayName = v.ID
	}
	if err := catalogCapabilities(raw, auth, &m); err != nil {
		return DiscoveredModel{}, false, err
	}
	if m.SupportsResponses != nil && *m.SupportsResponses {
		m.API = APIResponses
	} else if auth == AuthGitHubCopilot && m.SupportsMessages != nil && *m.SupportsMessages && m.ID == copilotMessagesModel && (m.SupportsReasoning == nil || *m.SupportsReasoning) {
		m.API = APIAnthropicMessages
	}
	m.Reference = modelReference(auth, m.ID)
	return m, true, nil
}

func publicProviderError(err error) error {
	var me *modelError
	if errors.As(err, &me) {
		return &Error{Code: CodeProviderError, Message: me.msg, ProviderFailure: me.failure}
	}
	return err
}
