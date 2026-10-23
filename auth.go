package sam

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Subscription authentication modes. API-key providers leave Auth empty.
const (
	AuthOpenAIChatGPT = "openai-chatgpt"
	AuthGitHubCopilot = "github-copilot"
	userAgent         = "sam-g"
)

// AuthConfig configures a separate, exclusively owned authentication database.
// It must not be the Runtime conversation database. Clients remain host-owned.
type AuthConfig struct {
	DatabasePath  string             `json:"database_path"`
	OpenAI        *OpenAIChatGPTAuth `json:"openai,omitempty"`
	GitHubCopilot *GitHubCopilotAuth `json:"github_copilot,omitempty"`
	HTTPClient    *http.Client       `json:"-"`
}

// OpenAIChatGPTAuth enables direct Sign in with ChatGPT. AgentName must be the
// actual host application's name, used consistently across installations.
type OpenAIChatGPTAuth struct {
	AgentName string `json:"agent_name"`
}

// GitHubCopilotAuth enables device login using Pi's Copilot compatibility
// profile. Empty ClientID uses DefaultGitHubCopilotClientID; a nonempty value
// explicitly overrides the public app registration. Provider access still
// depends on the account, subscription and organization policy.
type GitHubCopilotAuth struct {
	ClientID string `json:"client_id,omitempty"`
}

// AuthManager owns credentials, registrations, limits and mutation receipts in
// one private SQLite database. Several runtimes may share it. Close those
// runtimes first. The manager opens no listener and launches no browser.
type AuthManager struct {
	cfg        AuthConfig
	path       string
	lock       *os.File
	db         *sql.DB
	hostID     string
	hc         *http.Client
	unit       time.Duration
	ctx        context.Context
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	mu         sync.Mutex
	accounts   map[string]*account
	logins     map[string]*login
	receipts   map[string]authReceipt
	operations map[string]chan struct{}
	closed     bool
	broken     error
	stopped    chan struct{}
	closeErr   error
}

type account struct {
	cred    credential // guarded by manager mutex; published only after commit
	refresh *refreshWork
}

type refreshWork struct {
	cancel    context.CancelFunc
	done      chan struct{}
	err       error
	latest    *credential // retained even when retirement prevents normal publication
	uncertain bool
}

type registration struct {
	ClientID       string `json:"client_id,omitempty"`
	Issuer         string `json:"issuer,omitempty"`
	Subject        string `json:"subject,omitempty"`
	Email          string `json:"email,omitempty"`
	RedirectScheme string `json:"redirect_scheme,omitempty"`
	RedirectHost   string `json:"redirect_host,omitempty"`
	RedirectPath   string `json:"redirect_path,omitempty"`
}

type planState struct {
	Blocked    bool             `json:"blocked"`
	ResetAt    *time.Time       `json:"reset_at,omitempty"`
	ObservedAt *time.Time       `json:"observed_at,omitempty"`
	Failure    *ProviderFailure `json:"failure,omitempty"`
}

// credential is private. Access-token metadata is intentionally opaque.
type credential struct {
	Provider               string          `json:"provider"`
	Generation             string          `json:"generation"`
	Status                 string          `json:"status"`
	Created                time.Time       `json:"created_at"`
	Updated                time.Time       `json:"updated_at"`
	Profile                registration    `json:"profile"`
	Plan                   planState       `json:"plan"`
	Access                 string          `json:"access_token,omitempty"`
	Refresh                string          `json:"refresh_token,omitempty"`
	IDToken                string          `json:"id_token,omitempty"`
	TokenType              string          `json:"token_type,omitempty"`
	Scopes                 []string        `json:"scopes,omitempty"`
	Expires                time.Time       `json:"expires_at"`
	Received               time.Time       `json:"received_at"`
	EarliestRefresh        json.RawMessage `json:"earliest_refresh_at,omitempty"`
	RotationUncertain      bool            `json:"rotation_uncertain,omitempty"`
	AuthorizationUncertain bool            `json:"authorization_uncertain,omitempty"`
	GitHub                 string          `json:"github_token,omitempty"`
	Endpoint               string          `json:"endpoint,omitempty"`
	PendingOperation       string          `json:"pending_operation,omitempty"`
	LastTransferID         string          `json:"last_transfer_id,omitempty"`
	LastTransferStatus     string          `json:"last_transfer_status,omitempty"`
}

func (c credential) secrets() []string { return []string{c.Access, c.Refresh, c.IDToken, c.GitHub} }
func (c credential) stale(rejected string) bool {
	renewAt := c.Expires
	if c.Provider == AuthGitHubCopilot {
		renewAt = renewAt.Add(-copilotRefreshAhead)
	}
	return !time.Now().Before(renewAt) || (rejected != "" && c.Access == rejected)
}
func (c credential) usable() bool { return c.Status == "signed_in" && c.Access != "" }
func (c credential) planEnabled() bool {
	return slices.Contains(c.Scopes, "chatgpt.tokens.use.direct")
}

func (c *credential) clearTokens() {
	c.Access, c.Refresh, c.IDToken, c.GitHub = "", "", "", ""
	c.Scopes = nil
	c.Expires = time.Time{}
	c.EarliestRefresh = nil
	c.Status = "signed_out"
	c.PendingOperation = ""
	c.Updated = time.Now().UTC()
}

type accountRef struct{ name, generation, clientID string }

// AccountInfo is current safe state, not the outcome of a past receipt.
// PlanResetAt is unknown unless a documented provider fact supplies it.
type AccountInfo struct {
	Name               string           `json:"name"`
	Provider           string           `json:"provider"`
	Status             string           `json:"status"`
	ClientID           string           `json:"client_id,omitempty"`
	Issuer             string           `json:"issuer,omitempty"`
	Subject            string           `json:"subject,omitempty"`
	Email              string           `json:"email,omitempty"`
	PlanUsageEnabled   bool             `json:"plan_usage_enabled"`
	PlanBlocked        bool             `json:"plan_blocked"`
	PlanResetAt        *time.Time       `json:"plan_reset_at,omitempty"`
	PlanFailure        *ProviderFailure `json:"plan_failure,omitempty"`
	PendingOperation   string           `json:"pending_operation,omitempty"`
	LastTransferID     string           `json:"last_transfer_id,omitempty"`
	LastTransferStatus string           `json:"last_transfer_status,omitempty"`
	ExpiresAt          time.Time        `json:"expires_at"`
	CreatedAt          time.Time        `json:"created_at"`
	UpdatedAt          time.Time        `json:"updated_at"`
}

func accountInfo(name string, c credential) AccountInfo {
	// Clone pointer-bearing public metadata so a caller cannot change the gate.
	var p planState
	_ = json.Unmarshal([]byte(mustJSON(c.Plan)), &p)
	return AccountInfo{Name: name, Provider: c.Provider, Status: c.Status, ClientID: c.Profile.ClientID, Issuer: c.Profile.Issuer, Subject: c.Profile.Subject, Email: c.Profile.Email, PlanUsageEnabled: c.planEnabled(), PlanBlocked: p.Blocked, PlanResetAt: p.ResetAt, PlanFailure: p.Failure, PendingOperation: c.PendingOperation, LastTransferID: c.LastTransferID, LastTransferStatus: c.LastTransferStatus, ExpiresAt: c.Expires, CreatedAt: c.Created, UpdatedAt: c.Updated}
}

func (c *AuthConfig) normalize() error {
	if c.DatabasePath == "" {
		return errorf(CodeInvalidArgument, "auth database_path is required")
	}
	if c.OpenAI != nil {
		v := *c.OpenAI
		if strings.TrimSpace(v.AgentName) == "" || !utf8.ValidString(v.AgentName) {
			return errorf(CodeInvalidArgument, "auth openai.agent_name must be the application's actual nonblank name")
		}
		c.OpenAI = &v
	}
	if c.GitHubCopilot != nil {
		v := *c.GitHubCopilot
		if strings.TrimSpace(v.ClientID) == "" {
			v.ClientID = DefaultGitHubCopilotClientID
		}
		c.GitHubCopilot = &v
	}
	return nil
}

func validAccountName(name string) bool {
	return name != "" && strings.Trim(name, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._-") == ""
}

// Close cancels and joins manager-owned login, refresh and retirement work,
// then closes SQLite and releases exclusive ownership. All callers join it.
func (m *AuthManager) Close() error {
	m.mu.Lock()
	if m.stopped == nil {
		m.closed = true
		m.stopped = make(chan struct{})
		m.cancel()
		go func() {
			m.wg.Wait()
			m.mu.Lock()
			m.closeErr = errors.Join(m.broken, storageErr(m.db.Close()), storageErr(m.lock.Close()))
			m.mu.Unlock()
			close(m.stopped)
		}()
	}
	stopped := m.stopped
	m.mu.Unlock()
	<-stopped
	return m.closeErr
}

// Accounts returns retained registrations in label order, without tokens.
func (m *AuthManager) Accounts() []AccountInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []AccountInfo{}
	for _, name := range sortedKeys(m.accounts) {
		out = append(out, accountInfo(name, m.accounts[name].cred))
	}
	return out
}

func (m *AuthManager) kindOf(name string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if a := m.accounts[name]; a != nil {
		return a.cred.Provider, true
	}
	return "", false
}

func (m *AuthManager) hasAccounts(provider string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, a := range m.accounts {
		if a.cred.Provider == provider && a.cred.usable() {
			return true
		}
	}
	return false
}

func (m *AuthManager) ref(provider, name string) (accountRef, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := m.accounts[name]
	switch {
	case m.closed:
		return accountRef{}, errAuthClosed
	case m.broken != nil:
		return accountRef{}, m.broken
	case a == nil || !a.cred.usable():
		return accountRef{}, errorf(CodeAuthRequired, "account %q is not signed in; authorize that saved account explicitly", name)
	case a.cred.Provider != provider:
		return accountRef{}, errorf(CodeAuthRequired, "account %q belongs to another provider", name)
	}
	return accountRef{name: name, generation: a.cred.Generation, clientID: a.cred.Profile.ClientID}, nil
}

func (m *AuthManager) current(ref accountRef) (*account, credential, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.currentLocked(ref)
}

func (m *AuthManager) currentLocked(ref accountRef) (*account, credential, error) {
	a := m.accounts[ref.name]
	switch {
	case m.closed:
		return nil, credential{}, errAuthClosed
	case m.broken != nil:
		return nil, credential{}, m.broken
	case a == nil || !a.cred.usable() || a.cred.Generation != ref.generation:
		return nil, credential{}, errorf(CodeAuthRequired, "account %q was signed out or replaced; this operation cannot switch logins", ref.name)
	}
	return a, a.cred, nil
}

// credential registers one owned refresh. Its initiating caller controls I/O
// and joins settlement, preserving known errors during cancellation. Other
// waiters may detach; logout/export/Close can always cancel and join the owner.
func (m *AuthManager) credential(ctx context.Context, ref accountRef, rejected string) (credential, error) {
	m.mu.Lock()
	a, c, err := m.currentLocked(ref)
	if err != nil || !c.stale(rejected) {
		m.mu.Unlock()
		return c, err
	}
	w := a.refresh
	owner := w == nil
	if owner {
		// SIWC can rotate the saved refresh token. Record that crash gap before
		// provider I/O, retaining c as the pre-attempt uncertainty snapshot.
		if c.Provider == AuthOpenAIChatGPT && m.cfg.OpenAI != nil && c.Refresh != "" {
			marked := c
			marked.RotationUncertain = true
			if err := m.commit(ref.name, &marked, false, nil); err != nil {
				m.mu.Unlock()
				return credential{}, err
			}
		}
		rctx, cancel := context.WithCancel(ctx)
		stop := context.AfterFunc(m.ctx, cancel)
		w = &refreshWork{cancel: cancel, done: make(chan struct{})}
		a.refresh = w
		m.wg.Go(func() { defer stop(); m.runRefresh(rctx, ref, a, c, w) })
	}
	m.mu.Unlock()
	if owner {
		<-w.done
	} else {
		select {
		case <-ctx.Done():
			return credential{}, context.Cause(ctx)
		case <-w.done:
		}
	}
	if w.err != nil {
		return credential{}, refreshError(ref.name, w.err)
	}
	_, c, err = m.current(ref)
	if err == nil && !time.Now().Before(c.Expires) {
		return credential{}, errorf(CodeProviderError, "renewed credential expired during validation or persistence; no request was sent. Try again when renewal can finish within the provider's token lifetime")
	}
	return c, err
}

func (m *AuthManager) runRefresh(ctx context.Context, ref accountRef, a *account, c credential, w *refreshWork) {
	defer w.cancel()
	next, uncertain, err := m.refresh(ctx, c)
	m.mu.Lock()
	defer m.mu.Unlock()
	defer close(w.done)
	if err == nil {
		next.Provider, next.Generation, next.Created, next.Updated = c.Provider, c.Generation, c.Created, time.Now().UTC()
		next.Plan = c.Plan
		// A refresh can replace one known session, but cannot resolve a
		// separate authorization whose outcome may have created another one.
		next.AuthorizationUncertain = a.cred.AuthorizationUncertain
		next.LastTransferID = c.LastTransferID
		next.LastTransferStatus = c.LastTransferStatus
		next.Status = "signed_in"
		if m.accounts[ref.name] == a {
			// Save validated credentials even during retirement, before clearing
			// the rotation marker or using the latest token for revocation.
			next.Plan = a.cred.Plan
			next.Status, next.PendingOperation = a.cred.Status, a.cred.PendingOperation
			err = m.commit(ref.name, &next, false, nil)
			if err == nil {
				w.latest = &next
			}
		}
	} else {
		w.uncertain = c.RotationUncertain || uncertain
		if m.accounts[ref.name] == a && a.cred.RotationUncertain != w.uncertain {
			next = a.cred
			next.RotationUncertain = w.uncertain
			if saveErr := m.commit(ref.name, &next, false, nil); saveErr != nil {
				err = saveErr
			}
		}
		if !uncertain && terminalRefresh(err) && m.accounts[ref.name] == a && a.cred.usable() {
			next = a.cred
			next.clearTokens()
			if saveErr := m.commit(ref.name, &next, false, nil); saveErr != nil {
				err = saveErr
			}
		}
	}
	w.err = err
	// Retiring owners retain the settled work until they consume its latest token.
	if a.cred.usable() {
		a.refresh = nil
	}
}

// refresh returns this attempt's rotation uncertainty, not the account's prior
// fact. Issuance makes all subsequent validation failures uncertain regardless
// of their HTTP status; Copilot's GitHub-token exchange never rotates it.
func (m *AuthManager) refresh(ctx context.Context, c credential) (credential, bool, error) {
	if c.Provider == AuthOpenAIChatGPT {
		if m.cfg.OpenAI == nil || c.Refresh == "" {
			return credential{}, false, errorf(CodeAuthRequired, "this registration cannot renew; authorize it again")
		}
		next, issued, err := m.openaiToken(ctx, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {c.Refresh}, "client_id": {c.Profile.ClientID}, "resource": {openaiResource}}, c.Profile, "", &c)
		return next, err != nil && (issued || refreshOutcomeUncertain(err)), err
	}
	if m.cfg.GitHubCopilot == nil {
		return credential{}, false, errorf(CodeAuthRequired, "github-copilot login is not configured")
	}
	next, err := m.copilotToken(ctx, c.GitHub)
	return next, false, err
}

// Used only before issuance: a token-endpoint HTTP rejection is a known
// refusal. Downstream validation/JWKS failures cannot establish non-rotation.
func refreshOutcomeUncertain(err error) bool {
	f := providerFailureOf(err)
	return f == nil || f.HTTPStatus < 300
}

func terminalRefresh(err error) bool {
	var e *Error
	if !errors.As(err, &e) || e.ProviderFailure == nil {
		return false
	}
	switch e.ProviderFailure.Code {
	case "invalid_grant", "invalid_refresh_token", "expired_token", "token_expired", "refresh_token_expired", "refresh_token_invalidated", "refresh_token_reused":
		return true
	}
	return false
}

func refreshError(name string, err error) error {
	var me *modelError
	if errors.As(err, &me) {
		copy := *me
		copy.msg = "refreshing account " + name + ": " + me.msg
		return &copy
	}
	var e *Error
	if errors.As(err, &e) {
		copy := *e
		copy.Message = "refreshing account " + name + ": " + e.Message
		return &copy
	}
	return err
}

func (m *AuthManager) flowConfigured(provider string) bool {
	switch provider {
	case AuthOpenAIChatGPT:
		return m.cfg.OpenAI != nil
	case AuthGitHubCopilot:
		return m.cfg.GitHubCopilot != nil
	}
	return false
}

var knownAuth = []string{AuthOpenAIChatGPT, AuthGitHubCopilot}

func validAuth(provider string) bool { return slices.Contains(knownAuth, provider) }

var errAuthClosed = errorf(CodeClosed, "the auth manager is closed")
