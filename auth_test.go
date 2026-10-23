package sam

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestCopilotEndpointValidation(t *testing.T) {
	for raw, ok := range map[string]bool{"https://api.individual.githubcopilot.com": true, "https://githubcopilot.com/": true, "http://api.githubcopilot.com": false, "https://githubcopilot.com.evil.example": false, "https://api.githubcopilot.com:8443": false, "https://user@api.githubcopilot.com": false, "https://api.githubcopilot.com/v1?x=1": false, "https://evilgithubcopilot.com": false} {
		if _, err := copilotEndpoint(raw); (err == nil) != ok {
			t.Error(raw, err)
		}
		if _, err := normalizeProvider(Provider{Auth: AuthGitHubCopilot, BaseURL: raw}); (err == nil) != ok {
			t.Error("explicit", raw, err)
		}
	}
	if got := proxyEndpoint("tid=1;proxy-ep=proxy.enterprise.githubcopilot.com;x=y"); got != "https://api.enterprise.githubcopilot.com" {
		t.Fatal(got)
	}
}

func jsonResponse(status int, body any) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(mustJSON(body)))}
}

func TestExactProviderCodesBeforeGenericRetry(t *testing.T) {
	for _, code := range []string{"subscription_sharing_usage_limit_exceeded", "subscription_sharing_user_not_eligible", "subscription_sharing_unsupported_capability", "subscription_sharing_route_not_supported", "subscription_sharing_invalid_user", "chatpass_v2_scope_not_authorized"} {
		if e := providerModelError(&ProviderFailure{Code: code, HTTPStatus: 503}); e.retryable {
			t.Error(code)
		}
	}
	for _, code := range []string{"subscription_sharing_usage_unavailable", "subscription_sharing_user_unavailable"} {
		if e := providerModelError(&ProviderFailure{Code: code, HTTPStatus: 503}); !e.retryable {
			t.Error(code)
		}
	}
	e := statusError(jsonResponse(401, map[string]string{"detail": "direct permission was not accepted"}))
	if e.failure.Shape != "detail" || e.failure.Code != "" || e.failure.Message != "direct permission was not accepted" || e.retryable {
		t.Fatal(e.failure)
	}
	oversized := statusError(jsonResponse(429, map[string]any{"error": map[string]string{"code": "subscription_sharing_usage_limit_exceeded", "message": strings.Repeat("x", 5000)}}))
	if oversized.retryable || oversized.failure.Shape != "unknown" || oversized.failure.Code != "" {
		t.Fatal("truncated metadata guessed into a retry or limit fact", oversized.failure)
	}
}
