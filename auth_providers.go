package sam

// GitHub device login and shared bounded authentication transport.
// Direct Copilot route entitlement remains externally unverified.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// Engineering bounds, not provider guarantees.
	maxAuthBody = 1 << 20
	// RFC 8628 default when no polling interval is supplied.
	defaultPollSeconds = 5
	openaiIssuer       = "https://auth.openai.com"
	githubOrigin       = "https://github.com"
	githubAPIOrigin    = "https://api.github.com"
	jsonType           = "application/json"
	formType           = "application/x-www-form-urlencoded"
)

// noRedirectClient preserves host transport/settings without changing its client.
// Returning the response makes redirects explicit failures, without echoing Location.
func noRedirectClient(host *http.Client) *http.Client {
	if host == nil {
		host = http.DefaultClient
	}
	c := *host
	// Auth/discovery use only explicitly selected credentials, never ambient
	// browser cookies (including on public JWKS requests).
	c.Jar = nil
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &c
}

type authResponse struct {
	status          int
	raw             []byte
	retryAfter      time.Duration
	retryAfterKnown bool
	requestID       string
	secrets         []string
}

func (m *AuthManager) send(ctx context.Context, method, target, contentType, body string, header http.Header) (authResponse, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, strings.NewReader(body))
	if err != nil {
		return authResponse{}, errorf(CodeProviderError, "cannot construct authentication request")
	}
	if header != nil {
		req.Header = header.Clone()
	}
	req.Header.Set("Accept", jsonType)
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", userAgent)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := m.hc.Do(req)
	if err != nil {
		if identifiableCancellation(ctx, err) {
			return authResponse{}, context.Cause(ctx)
		}
		return authResponse{}, &modelError{msg: "authentication transport failed; check connectivity and try a new login if needed", retryable: true}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxAuthBody+1))
	if len(raw) > maxAuthBody {
		return authResponse{}, errorf(CodeProviderError, "authentication response exceeds 1 MiB")
	}
	if err != nil {
		if identifiableCancellation(ctx, err) {
			return authResponse{}, context.Cause(ctx)
		}
		return authResponse{}, &modelError{msg: "reading authentication response failed", retryable: true}
	}
	delay, known := parseRetryAdvice(resp.Header.Get("Retry-After"), time.Now())
	secrets := authRequestSecrets(body, header)
	return authResponse{status: resp.StatusCode, raw: raw, retryAfter: delay, retryAfterKnown: known, requestID: diagnostic(resp.Header.Get("x-request-id"), secrets...), secrets: secrets}, nil
}

// Error fields are bounded and redacted; free-form OAuth descriptions and
// successful token bodies never become diagnostics.
func (r authResponse) failure(what string) error {
	msg := fmt.Sprintf("%s failed (HTTP %d)", what, r.status)
	code := oauthErrorCode(r.raw)
	switch code {
	case "invalid_grant", "invalid_client", "invalid_refresh_token", "refresh_token_expired", "refresh_token_invalidated", "refresh_token_reused", "unauthorized_client", "access_denied", "expired_token", "token_expired", "device_flow_disabled", "slow_down", "authorization_pending":
		msg += ": " + code
	}
	code = diagnostic(code, r.secrets...)
	if r.status >= 300 && r.status < 400 {
		msg += "; unexpected redirect refused to protect credentials"
	}
	var envelope struct {
		Error json.RawMessage `json:"error"`
	}
	var wire wireFailure
	shape := "unknown"
	if json.Unmarshal(r.raw, &envelope) == nil && len(envelope.Error) > 0 {
		shape = "error"
		_ = json.Unmarshal(envelope.Error, &wire)
	}
	f := &ProviderFailure{HTTPStatus: r.status, Code: code, Param: diagnostic(wire.Param, r.secrets...), RequestID: r.requestID, Message: msg, Shape: shape}
	if r.retryAfterKnown {
		f.RetryAfterSeconds = new(r.retryAfter.Seconds())
	}
	if (r.status == 429 || r.status >= 500) && !permanentOAuth(code) {
		f.Retryable = true
		return &modelError{msg: msg, retryable: true, retryAfter: r.retryAfter, failure: f}
	}
	outer := CodeProviderError
	if code == "invalid_grant" || code == "invalid_refresh_token" || strings.HasPrefix(code, "refresh_token_") || code == "expired_token" || code == "token_expired" {
		outer = CodeAuthRequired
	}
	return &Error{Code: outer, Message: msg, ProviderFailure: f}
}

func permanentOAuth(code string) bool {
	switch code {
	case "invalid_grant", "invalid_client", "invalid_refresh_token", "refresh_token_expired", "refresh_token_invalidated", "refresh_token_reused", "unauthorized_client", "access_denied", "expired_token", "token_expired", "device_flow_disabled":
		return true
	}
	return false
}

func authRequestSecrets(body string, header http.Header) []string {
	form, _ := url.ParseQuery(body)
	secrets := []string{form.Get("code"), form.Get("code_verifier"), form.Get("refresh_token"), form.Get("token")}
	if _, token, ok := strings.Cut(header.Get("Authorization"), " "); ok {
		secrets = append(secrets, token)
	}
	return secrets
}

func oauthErrorCode(raw []byte) string {
	var v struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(raw, &v) != nil {
		return ""
	}
	var s string
	if json.Unmarshal(v.Error, &s) == nil {
		return s
	}
	var o struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(v.Error, &o) != nil {
		return ""
	}
	return o.Code
}

// seconds validates representation before conversion. Missing values use def;
// null, non-finite, negative and overflowing durations are malformed.
func seconds(raw json.RawMessage, def float64) (float64, bool) {
	n := def
	if len(raw) > 0 {
		if string(raw) == "null" {
			return 0, false
		}
		if json.Unmarshal(raw, &n) != nil {
			var s string
			if json.Unmarshal(raw, &s) != nil {
				return 0, false
			}
			var err error
			n, err = strconv.ParseFloat(s, 64)
			if err != nil {
				return 0, false
			}
		}
	}
	return n, !math.IsNaN(n) && !math.IsInf(n, 0) && n >= 0 && (n == 0 || n*float64(time.Second) >= 1) && n*float64(time.Second) < float64(math.MaxInt64)
}

func (m *AuthManager) secs(n float64) time.Duration {
	if n == 0 {
		n = defaultPollSeconds
	}
	return time.Duration(n * float64(m.unit))
}

func (m *AuthManager) copilotDevice(ctx context.Context) (*device, error) {
	cfg := m.cfg.GitHubCopilot
	resp, err := m.send(ctx, http.MethodPost, githubOrigin+"/login/device/code", formType, url.Values{"client_id": {cfg.ClientID}, "scope": {"read:user"}}.Encode(), copilotDeviceHeaders())
	if err != nil {
		return nil, err
	}
	if code := oauthErrorCode(resp.raw); resp.status != http.StatusOK || code != "" {
		if code == "device_flow_disabled" {
			return nil, errorf(CodeUnsupported, "GitHub device-code login is disabled for this client_id (device_flow_disabled)")
		}
		return nil, resp.failure("GitHub device-code request")
	}
	var r struct {
		DeviceCode string          `json:"device_code"`
		UserCode   string          `json:"user_code"`
		URI        string          `json:"verification_uri"`
		ExpiresIn  json.RawMessage `json:"expires_in"`
		Interval   json.RawMessage `json:"interval"`
	}
	err = json.Unmarshal(resp.raw, &r)
	interval, okI := seconds(r.Interval, defaultPollSeconds)
	life, okE := seconds(r.ExpiresIn, 0)
	if err != nil || r.DeviceCode == "" || r.UserCode == "" || !okI || !okE || time.Duration(life*float64(time.Second)) <= 0 || !sameOrigin(r.URI, githubOrigin) {
		return nil, errorf(CodeProviderError, "GitHub device-code response is malformed or names an unexpected verification URL")
	}
	d := &device{url: r.URI, code: r.UserCode, interval: m.secs(interval), expires: time.Now().Add(time.Duration(life * float64(time.Second)))}
	d.poll = func(ctx context.Context) (pollAnswer, error) {
		form := url.Values{"client_id": {cfg.ClientID}, "device_code": {r.DeviceCode}, "grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}}
		resp, err := m.send(ctx, http.MethodPost, githubOrigin+"/login/oauth/access_token", formType, form.Encode(), copilotDeviceHeaders())
		if err != nil {
			return pollAnswer{}, err
		}
		var t struct {
			Access   string          `json:"access_token"`
			Error    string          `json:"error"`
			Interval json.RawMessage `json:"interval"`
		}
		if resp.status != http.StatusOK || json.Unmarshal(resp.raw, &t) != nil {
			return pollAnswer{}, resp.failure("GitHub device-code poll")
		}
		switch t.Error {
		case "":
			if t.Access == "" {
				return pollAnswer{}, errorf(CodeProviderError, "GitHub device-code token response is malformed")
			}
			cred, err := m.copilotToken(ctx, t.Access)
			return pollAnswer{credential: &cred}, err
		case "authorization_pending":
			return pollAnswer{}, nil
		case "slow_down":
			n, ok := seconds(t.Interval, 0)
			if !ok {
				return pollAnswer{}, errorf(CodeProviderError, "GitHub slow_down interval is malformed")
			}
			return pollAnswer{slowDown: true, interval: time.Duration(n * float64(m.unit))}, nil
		case "expired_token":
			return pollAnswer{}, errLoginExpired
		case "access_denied":
			return pollAnswer{}, errorf(CodeAuthRequired, "GitHub authorization was denied")
		}
		return pollAnswer{}, resp.failure("GitHub device-code poll")
	}
	return d, nil
}

func (m *AuthManager) copilotToken(ctx context.Context, gh string) (credential, error) {
	header := copilotHeaders()
	header.Set("Authorization", "Bearer "+gh)
	resp, err := m.send(ctx, http.MethodGet, githubAPIOrigin+"/copilot_internal/v2/token", "", "", header)
	if err != nil {
		return credential{}, err
	}
	switch resp.status {
	case http.StatusUnauthorized:
		return credential{}, errorf(CodeAuthRequired, "GitHub rejected the saved authorization (HTTP 401). Log in to this account again")
	case http.StatusForbidden, http.StatusNotFound:
		return credential{}, errorf(CodeAuthRequired, "GitHub did not grant Copilot access (HTTP %d). Check the account's subscription, organization policy, and client permission", resp.status)
	case http.StatusOK:
	default:
		return credential{}, resp.failure("Copilot token request")
	}
	var t struct {
		Token     string `json:"token"`
		ExpiresAt int64  `json:"expires_at"`
		Endpoints struct {
			API string `json:"api"`
		} `json:"endpoints"`
	}
	// time.Time's JSON/RFC3339 representation ends at year 9999, not an
	// arbitrary product lifetime limit. Expiry must also still be in the future.
	if json.Unmarshal(resp.raw, &t) != nil || t.Token == "" || t.ExpiresAt <= time.Now().Unix() || t.ExpiresAt > 253402300799 {
		return credential{}, errorf(CodeProviderError, "Copilot token response is malformed (token or expiry)")
	}
	// Pi prefers the token's proxy-ep and falls back to the Individual API.
	// Retain our stricter validation of any supplied endpoint metadata too.
	if t.Endpoints.API != "" {
		if _, err := copilotEndpoint(t.Endpoints.API); err != nil {
			return credential{}, err
		}
	}
	endpoint := proxyEndpoint(t.Token)
	if endpoint == "" {
		endpoint = t.Endpoints.API
	}
	if endpoint == "" {
		endpoint = copilotDefaultAPI
	}
	if endpoint, err = copilotEndpoint(endpoint); err != nil {
		return credential{}, err
	}
	return credential{Access: t.Token, Expires: time.Unix(t.ExpiresAt, 0).UTC(), Received: time.Now().UTC(), GitHub: gh, Endpoint: endpoint}, nil
}

func proxyEndpoint(token string) string {
	for part := range strings.SplitSeq(token, ";") {
		if host, ok := strings.CutPrefix(part, "proxy-ep="); ok {
			if rest, ok := strings.CutPrefix(host, "proxy."); ok {
				host = "api." + rest
			}
			return "https://" + host
		}
	}
	return ""
}

func copilotEndpoint(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err == nil && u.Scheme == "https" && u.User == nil && u.Port() == "" && (u.Path == "" || u.Path == "/") && u.RawQuery == "" && u.Fragment == "" {
		if h := u.Hostname(); h == "githubcopilot.com" || strings.HasSuffix(h, ".githubcopilot.com") {
			return "https://" + h, nil
		}
	}
	return "", errorf(CodeProviderError, "Copilot reported an untrusted API endpoint; credentials may only be sent to https://*.githubcopilot.com. Check provider/client support before logging in again")
}

func sameOrigin(raw, base string) bool {
	u, err := url.Parse(raw)
	b, berr := url.Parse(base)
	return err == nil && berr == nil && u.Scheme == b.Scheme && u.Host == b.Host && u.User == nil
}
