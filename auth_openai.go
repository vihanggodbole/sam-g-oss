package sam

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

type browserAttempt struct {
	redirect, state, nonce, verifier, url string
	profile                               registration
	callback                              chan browserCallback
	consumed                              bool
}
type browserCallback struct {
	code, client string
	oauthError   string
}

// CompleteLoginRequest passes the complete URL received by the host's bound
// loopback listener. Construct it from that listener, never an untrusted Host.
type CompleteLoginRequest struct {
	LoginID     string `json:"login_id"`
	CallbackURL string `json:"callback_url"`
}

func validRedirect(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Path != "/auth/callback" || u.RawPath != "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return false
	}
	port, err := strconv.Atoi(u.Port())
	return err == nil && port > 0 && port <= 65535 && u.Host == "127.0.0.1:"+strconv.Itoa(port)
}

func randomOAuth() string {
	var b [32]byte
	_, _ = rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}

func (m *AuthManager) newBrowser(req LoginRequest) *browserAttempt {
	b := &browserAttempt{redirect: req.RedirectURI, state: randomOAuth(), nonce: randomOAuth(), verifier: randomOAuth(), callback: make(chan browserCallback, 1)}
	if a := m.accounts[req.Account]; a != nil {
		b.profile = a.cred.Profile
	}
	client := b.profile.ClientID
	if client == "" {
		client = "dynamic_agent_client"
	}
	sum := sha256.Sum256([]byte(b.verifier))
	q := url.Values{"client_id": {client}, "ext_agent_host_id": {m.hostID}, "response_type": {"code"}, "redirect_uri": {b.redirect}, "scope": {openaiScopes}, "resource": {openaiResource}, "state": {b.state}, "nonce": {b.nonce}, "code_challenge_method": {"S256"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}}
	if client == "dynamic_agent_client" {
		q.Set("agent_name_hint", m.cfg.OpenAI.AgentName)
	}
	if req.EnablePlanUsage {
		q.Set("prompt", "consent")
	}
	b.url = openaiIssuer + "/api/accounts/authorize?" + q.Encode()
	return b
}

// CompleteLogin consumes one valid callback. Once admitted, exchange and
// credential persistence belong to the manager; ctx cancels only this wait.
func (m *AuthManager) CompleteLogin(ctx context.Context, req CompleteLoginRequest) (LoginStatus, error) {
	if err := ctx.Err(); err != nil {
		return LoginStatus{}, err
	}
	m.mu.Lock()
	l := m.logins[req.LoginID]
	if m.closed {
		m.mu.Unlock()
		return LoginStatus{}, errAuthClosed
	}
	if l == nil || l.browser == nil {
		m.mu.Unlock()
		return LoginStatus{}, errorf(CodeNotFound, "pending browser login not found")
	}
	b := l.browser
	cb, err := validateCallback(b, req.CallbackURL)
	if err == nil && (!l.active() || b.consumed) {
		err = errorf(CodeConflict, "this login callback has already been consumed or cancelled")
	}
	if err != nil {
		m.mu.Unlock()
		return LoginStatus{}, err
	}
	b.consumed = true
	b.callback <- cb
	m.mu.Unlock()
	return m.WaitLogin(ctx, req.LoginID)
}

func validateCallback(b *browserAttempt, raw string) (browserCallback, error) {
	bad := errorf(CodeInvalidArgument, "callback URI, state or client identity does not match this login")
	u, err := url.Parse(raw)
	if err != nil || u.Fragment != "" || u.User != nil {
		return browserCallback{}, bad
	}
	base := *u
	base.RawQuery = ""
	base.ForceQuery = false
	if base.String() != b.redirect {
		return browserCallback{}, bad
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return browserCallback{}, bad
	}
	for _, key := range []string{"state", "code", "client_id", "error", "error_description", "scope"} {
		if len(q[key]) > 1 {
			return browserCallback{}, bad
		}
	}
	if q.Get("state") == "" || q.Get("state") != b.state {
		return browserCallback{}, bad
	}
	client := q.Get("client_id")
	if client != "" && (client == "dynamic_agent_client" || (b.profile.ClientID != "" && client != b.profile.ClientID)) {
		return browserCallback{}, bad
	}
	if q.Get("error") != "" {
		if q.Get("code") != "" {
			return browserCallback{}, bad
		}
		return browserCallback{oauthError: q.Get("error")}, nil
	}
	if client == "" {
		client = b.profile.ClientID
	}
	if strings.TrimSpace(q.Get("code")) == "" || strings.TrimSpace(client) == "" {
		return browserCallback{}, bad
	}
	return browserCallback{code: q.Get("code"), client: client}, nil
}

func (m *AuthManager) runBrowser(ctx context.Context, l *login) {
	defer close(l.stopped)
	defer l.cancel()
	m.mu.Lock()
	if !l.active() || ctx.Err() != nil {
		m.finishInitial(l, context.Canceled)
		close(l.ready)
		m.mu.Unlock()
		return
	}
	l.status.State = LoginPending
	rc := m.receipts[l.requestID]
	rc.Ack, rc.Status = durableLoginStatus(l.status), durableLoginStatus(l.status)
	err := m.commit("", nil, false, &rc)
	close(l.ready)
	b := l.browser
	m.mu.Unlock()
	if err != nil {
		m.finishLogin(ctx, l, credential{}, err, false)
		return
	}
	var cb browserCallback
	select {
	case <-ctx.Done():
		m.finishLogin(ctx, l, credential{}, context.Cause(ctx), false)
		return
	case cb = <-b.callback:
	}
	if cb.oauthError != "" {
		failure := errorf(CodeProviderError, "OpenAI authorization failed before token exchange; start a fresh login")
		if cb.oauthError == "access_denied" {
			failure = errorf(CodeAuthRequired, "OpenAI authorization was denied; no code was exchanged")
		}
		m.finishLogin(ctx, l, credential{}, failure, false)
		return
	}
	profile := b.profile
	profile.ClientID = cb.client
	// Persist issued-client metadata before consuming the code, without declaring
	// its unverified identity usable. A later attempt can reuse this client ID.
	if profile.Subject == "" {
		m.mu.Lock()
		c := credential{Provider: AuthOpenAIChatGPT, Status: "incomplete", Profile: profile, Created: time.Now().UTC()}
		if old := m.accounts[l.status.Account]; old != nil {
			c = old.cred
			c.Profile = profile
		}
		if l.active() && ctx.Err() == nil {
			err = m.commit(l.status.Account, &c, false, nil)
		} else {
			err = context.Canceled
		}
		m.mu.Unlock()
		if err != nil {
			m.finishLogin(ctx, l, credential{}, err, false)
			return
		}
	}
	m.mu.Lock()
	if !l.active() || ctx.Err() != nil {
		m.mu.Unlock()
		m.finishLogin(ctx, l, credential{}, context.Canceled, false)
		return
	}
	a := m.accounts[l.status.Account]
	if a == nil {
		c := credential{Provider: AuthOpenAIChatGPT, Status: "incomplete", Profile: profile, Created: time.Now().UTC()}
		err = m.commit(l.status.Account, &c, false, nil)
		a = m.accounts[l.status.Account]
	}
	if err == nil {
		l.priorAuthorizationUncertain = a.cred.AuthorizationUncertain
		c := a.cred
		c.AuthorizationUncertain = true
		err = m.commit(l.status.Account, &c, false, nil)
	}
	if err == nil {
		l.exchanging = true
	}
	m.mu.Unlock()
	if err != nil {
		m.finishLogin(ctx, l, credential{}, err, false)
		return
	}
	c, issued, err := m.openaiToken(ctx, url.Values{"grant_type": {"authorization_code"}, "code": {cb.code}, "client_id": {cb.client}, "code_verifier": {b.verifier}, "redirect_uri": {b.redirect}, "resource": {openaiResource}}, profile, b.nonce, nil)
	m.finishLogin(ctx, l, c, err, issued)
}

// openaiToken distinguishes a definite token-endpoint rejection from any
// later failure, after the provider may already have issued credentials.
func (m *AuthManager) openaiToken(ctx context.Context, form url.Values, profile registration, nonce string, old *credential) (credential, bool, error) {
	resp, err := m.send(ctx, http.MethodPost, openaiTokenEndpoint, formType, form.Encode(), nil)
	if err != nil {
		return credential{}, false, err
	}
	if resp.status != http.StatusOK {
		return credential{}, false, resp.failure("OpenAI token request")
	}
	received := time.Now().UTC() // JWKS validation must not extend the access lifetime.
	var t struct {
		Access   string          `json:"access_token"`
		Refresh  string          `json:"refresh_token"`
		IDToken  string          `json:"id_token"`
		Type     string          `json:"token_type"`
		Expires  json.RawMessage `json:"expires_in"`
		Scope    *string         `json:"scope"`
		Earliest json.RawMessage `json:"earliest_refresh_at"`
	}
	bad := errorf(CodeProviderError, "OpenAI token response has invalid credentials, token type, expiry or granted scopes; authorize again")
	if json.Unmarshal(resp.raw, &t) != nil || t.Access == "" || !strings.EqualFold(t.Type, "Bearer") || t.Scope == nil {
		return credential{}, true, bad
	}
	var expiry float64
	if len(t.Expires) == 0 || json.Unmarshal(t.Expires, &expiry) != nil {
		return credential{}, true, bad
	}
	if _, ok := seconds(t.Expires, 0); !ok || expiry <= 0 {
		return credential{}, true, bad
	}
	scopes := strings.Fields(*t.Scope)
	if !slices.Contains(scopes, "openid") || (slices.Contains(scopes, "offline_access") && t.Refresh == "") {
		return credential{}, true, bad
	}
	if old == nil && t.IDToken == "" {
		return credential{}, true, bad
	}
	verified := profile
	if t.IDToken != "" {
		verified, err = m.validateID(ctx, t.IDToken, profile.ClientID, nonce, false)
		if err != nil {
			return credential{}, true, err
		}
		if profile.Subject != "" && !sameRegistration(profile, verified) {
			return credential{}, true, errorf(CodeAuthRequired, "OpenAI identity does not match this saved registration; use its original identity or a new account label")
		}
	} else if old != nil {
		t.IDToken = old.IDToken
	}
	return credential{Profile: verified, Access: t.Access, Refresh: t.Refresh, IDToken: t.IDToken, TokenType: t.Type, Scopes: scopes, Expires: received.Add(time.Duration(expiry * float64(time.Second))), Received: received, EarliestRefresh: t.Earliest}, true, nil
}

func durableLoginStatus(s LoginStatus) LoginStatus { s.AuthorizationURL = ""; return s }
func (l *login) publicStatus() LoginStatus {
	s := l.status
	if l.browser != nil && l.active() && !l.browser.consumed {
		s.AuthorizationURL = l.browser.url
	}
	return s
}
