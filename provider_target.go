package sam

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// normalizeProvider is shared by standalone discovery and configured inference.
// Subscription trust never depends on an arbitrary configured HTTP endpoint.
func normalizeProvider(p Provider) (Provider, error) {
	p.BaseURL = strings.TrimRight(p.BaseURL, "/")
	if p.Auth != "" {
		if !validAuth(p.Auth) {
			return p, errorf(CodeInvalidArgument, "unknown subscription auth mode %q", p.Auth)
		}
		if p.APIKey != "" || p.APIKeyEnv != "" {
			return p, errorf(CodeInvalidArgument, "subscription auth cannot also configure API-key billing")
		}
	}
	switch p.Auth {
	case AuthOpenAIChatGPT:
		if p.BaseURL == "" {
			p.BaseURL = openaiResource
		}
		if p.BaseURL != openaiResource {
			return p, errorf(CodeInvalidArgument, "SIWC requires exactly https://api.openai.com/v1; legacy Codex conversations require a new session/database, not endpoint redirection")
		}
	case AuthGitHubCopilot:
		if p.BaseURL != "" {
			base, err := copilotEndpoint(p.BaseURL)
			if err != nil {
				return p, errorf(CodeInvalidArgument, "Copilot base_url must be an HTTPS githubcopilot.com origin without path, port or credentials")
			}
			p.BaseURL = base
		}
	default:
		u, err := url.Parse(p.BaseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return p, errorf(CodeInvalidArgument, "API-key base_url must be an HTTP(S) base URL without userinfo, query or fragment")
		}
	}
	return p, nil
}

func credentialTarget(ctx context.Context, p Provider, m *AuthManager, ref accountRef, rejected string) (endpoint, error) {
	p, err := normalizeProvider(p)
	if err != nil {
		return endpoint{}, err
	}
	h := http.Header{}
	if p.Auth == "" {
		key := p.APIKey
		if key == "" && p.APIKeyEnv != "" {
			key = os.Getenv(p.APIKeyEnv)
		}
		if key != "" {
			h.Set("Authorization", "Bearer "+key)
		}
		return endpoint{url: p.BaseURL, header: h, token: key, secrets: []string{key}}, nil
	}
	if m == nil {
		return endpoint{}, errorf(CodeAuthRequired, "subscription discovery/inference requires an AuthManager")
	}
	c, err := m.credential(ctx, ref, rejected)
	if err != nil {
		return endpoint{}, err
	}
	if c.Provider == AuthOpenAIChatGPT && !c.planEnabled() {
		return endpoint{}, errorf(CodeAuthRequired, "ChatGPT plan use is disabled; explicitly authorize with EnablePlanUsage")
	}
	base := p.BaseURL
	if p.Auth == AuthGitHubCopilot && base == "" {
		base, err = copilotEndpoint(c.Endpoint)
		if err != nil {
			return endpoint{}, err
		}
	}
	if p.Auth == AuthGitHubCopilot {
		h = copilotHeaders()
	} else {
		h.Set("User-Agent", userAgent)
	}
	h.Set("Authorization", "Bearer "+c.Access)
	return endpoint{url: base, header: h, token: c.Access, secrets: c.secrets(), subscription: true}, nil
}
