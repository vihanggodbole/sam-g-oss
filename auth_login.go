package sam

import (
	"cmp"
	"context"
	"errors"
	"math"
	"slices"
	"strings"
	"time"
)

// Login states reported by LoginStatus.State.
const (
	LoginStarting  = "starting" // initial device request is owned by the manager
	LoginPending   = "pending"  // waiting for browser authorization
	LoginCompleted = "completed"
	LoginFailed    = "failed"
	LoginCancelled = "cancelled"
	LoginExpired   = "expired"
)

// LoginRequest starts a browser-code SIWC or Copilot device login. Returning
// authorization may replace credentials only after identity validation.
// RequestID deduplicates acceptance; browser replay after termination/restart
// returns terminal status without reusable authorization data.
type LoginRequest struct {
	RequestID       string `json:"request_id"`
	Provider        string `json:"provider"`
	Account         string `json:"account"`
	RedirectURI     string `json:"redirect_uri,omitempty"`
	EnablePlanUsage bool   `json:"enable_plan_usage,omitempty"`
}

// LoginStatus is safe to show to the user. Tokens and private device codes are
// never included. Open VerificationURL and enter UserCode before ExpiresAt.
type LoginStatus struct {
	ID               string    `json:"id"`
	Provider         string    `json:"provider"`
	Account          string    `json:"account"`
	State            string    `json:"state"`
	Flow             string    `json:"flow"`
	AuthorizationURL string    `json:"authorization_url"`
	VerificationURL  string    `json:"verification_url"`
	UserCode         string    `json:"user_code"`
	ExpiresAt        time.Time `json:"expires_at"`
	Code             ErrorCode `json:"code,omitempty"`
	Error            string    `json:"error,omitempty"`
}

type login struct {
	status                      LoginStatus
	requestID                   string
	cancel                      context.CancelFunc
	ready                       chan struct{} // initial request and its acknowledgement settled
	stopped                     chan struct{} // initial request AND polling joined; also WaitLogin
	browser                     *browserAttempt
	exchanging                  bool
	priorAuthorizationUncertain bool
	latest                      *credential
	uncertain                   bool
}

// A terminal status may precede the owner's join (for example CancelLogin).
// Account retirement must join that work too, not only pending statuses.
func (l *login) unsettled() bool {
	select {
	case <-l.stopped:
		return false
	default:
		return true
	}
}

func (l *login) active() bool {
	return l.status.State == LoginStarting || l.status.State == LoginPending
}

// end changes state under mu; only the worker closes lifecycle channels.
func (l *login) end(state string, code ErrorCode, msg string) {
	if !l.active() {
		return
	}
	l.status.State, l.status.Code, l.status.Error = state, code, msg
	l.cancel()
}

type device struct {
	url, code string
	interval  time.Duration
	expires   time.Time
	poll      func(context.Context) (pollAnswer, error)
}

type pollAnswer struct {
	credential *credential
	slowDown   bool
	interval   time.Duration
}

// StartLogin reserves the login before any network I/O. Concurrent identical
// starts join that initial request. ctx cancels the initial request only; after
// its challenge is accepted the manager owns polling, independent of callers.
func (m *AuthManager) StartLogin(ctx context.Context, req LoginRequest) (LoginStatus, error) {
	if err := ctx.Err(); err != nil {
		return LoginStatus{}, err
	}
	m.mu.Lock()
	rc := authReceipt{RequestID: req.RequestID, Kind: "start", Target: req.Account, Provider: req.Provider}
	if req.RedirectURI != "" || req.EnablePlanUsage {
		rc.Identity = authIdentity(struct {
			Redirect string
			Consent  bool
		}{req.RedirectURI, req.EnablePlanUsage})
	}
	old, err := m.replay(rc)
	if old != nil || err != nil {
		var l *login
		if old != nil {
			l = m.logins[old.Status.ID]
		}
		m.mu.Unlock()
		if err != nil {
			if old != nil {
				return old.Ack, err
			}
			return LoginStatus{}, err
		}
		return m.loginAck(ctx, l)
	}
	l, lctx, err := m.reserveLogin(req, &rc)
	if err == nil {
		if l.browser != nil {
			m.wg.Go(func() { m.runBrowser(lctx, l) })
		} else {
			m.wg.Go(func() { m.runLogin(ctx, lctx, l) })
		}
	}
	m.mu.Unlock()
	if err != nil {
		return LoginStatus{}, err
	}
	return m.loginAck(ctx, l)
}

// reserveLogin is the only admission path; the global lock never spans I/O.
func (m *AuthManager) reserveLogin(req LoginRequest, rc *authReceipt) (*login, context.Context, error) {
	if !validAccountName(req.Account) {
		return nil, nil, errorf(CodeInvalidArgument, "account name must use only letters, digits, '.', '_' and '-'")
	}
	if !m.flowConfigured(req.Provider) {
		return nil, nil, errorf(CodeInvalidArgument, "login provider is not configured in the auth section")
	}
	if req.Provider == AuthOpenAIChatGPT {
		if !validRedirect(req.RedirectURI) {
			return nil, nil, errorf(CodeInvalidArgument, "OpenAI login requires an exact http://127.0.0.1:<port>/auth/callback URI from the host's listener")
		}
	} else if req.RedirectURI != "" || req.EnablePlanUsage {
		return nil, nil, errorf(CodeInvalidArgument, "redirect_uri and enable_plan_usage apply only to OpenAI")
	}
	if a := m.accounts[req.Account]; a != nil && a.cred.PendingOperation != "" {
		return nil, nil, errorf(CodeConflict, "account operation is still settling")
	}
	for _, l := range m.logins {
		if l.status.Account == req.Account && l.unsettled() {
			return nil, nil, errorf(CodeConflict, "a login for account %q is still settling; wait for completion or cancellation to finish", req.Account)
		}
	}
	if a := m.accounts[req.Account]; a != nil && a.cred.Provider != req.Provider {
		return nil, nil, errorf(CodeConflict, "account %q belongs to another provider; log it out first or choose another name", req.Account)
	}
	lctx, cancel := context.WithCancel(m.ctx)
	l := &login{requestID: req.RequestID, cancel: cancel, ready: make(chan struct{}), stopped: make(chan struct{}), status: LoginStatus{ID: newID("login"), Provider: req.Provider, Account: req.Account, State: LoginStarting}}
	l.status.Flow = "device_code"
	if req.Provider == AuthOpenAIChatGPT {
		l.status.Flow = "authorization_code"
		l.browser = m.newBrowser(req)
	}
	rc.Ack, rc.Status = l.status, l.status
	if err := m.commit("", nil, false, rc); err != nil {
		cancel()
		return nil, nil, err
	}
	m.logins[l.status.ID] = l
	return l, lctx, nil
}

func (m *AuthManager) loginAck(ctx context.Context, l *login) (LoginStatus, error) {
	select {
	case <-l.ready:
		if ctx.Err() != nil {
			return LoginStatus{}, ctx.Err()
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.broken != nil {
			return LoginStatus{}, m.broken
		}
		rc := m.receipts[l.requestID]
		if rc.Failure != nil {
			e := *rc.Failure
			return rc.Ack, &e
		}
		if l.browser != nil || l.status.Flow == "authorization_code" {
			return l.publicStatus(), nil
		}
		return rc.Ack, nil
	case <-ctx.Done():
		return LoginStatus{}, ctx.Err()
	}
}

func (m *AuthManager) runLogin(caller, lctx context.Context, l *login) {
	defer close(l.stopped)
	defer l.cancel()
	// The caller controls only initial I/O, not the polling context.
	initial, cancel := context.WithCancel(lctx)
	stop := context.AfterFunc(caller, cancel)
	if caller.Err() != nil {
		cancel()
	}
	d, err := m.copilotDevice(initial)
	stop()
	m.mu.Lock()
	if initial.Err() != nil && err == nil {
		err = context.Cause(initial)
	}
	cancel()
	if !l.active() || lctx.Err() != nil || err != nil {
		m.finishInitial(l, err)
		close(l.ready)
		m.mu.Unlock()
		return
	}
	l.status.State, l.status.VerificationURL, l.status.UserCode, l.status.ExpiresAt = LoginPending, d.url, d.code, d.expires.UTC()
	rc := m.receipts[l.requestID]
	rc.Ack, rc.Status = l.status, l.status
	err = m.commit("", nil, false, &rc)
	if err != nil {
		l.end(LoginFailed, CodeStorageError, err.Error())
	}
	close(l.ready)
	m.mu.Unlock()
	if err != nil {
		return
	}
	ctx, cancelPoll := context.WithDeadlineCause(lctx, d.expires, errLoginExpired)
	defer cancelPoll()
	cred, err := m.authorize(ctx, d)
	m.finishLogin(ctx, l, cred, err, false)
}

// finishInitial records a safe failure/ack even if logout cancelled the
// reservation while its device request was still in flight. Caller holds mu.
func (m *AuthManager) finishInitial(l *login, err error) {
	rc := m.receipts[l.requestID]
	if l.active() {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || m.ctx.Err() != nil {
			l.end(LoginCancelled, "", "initial login request cancelled; use a new request_id to start again")
		} else {
			e := publicLoginError(err)
			l.end(LoginFailed, e.Code, e.Message)
			rc.Failure = e
		}
	}
	rc.Ack, rc.Status = l.status, l.status
	_ = m.commit("", nil, false, &rc) // commit sets the unusable-store boundary on failure
}

func publicLoginError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		copy := *e
		return &copy
	}
	return errorf(CodeProviderError, "%s", loginMessage(err))
}

var errLoginExpired = errors.New("device code expired")

func (m *AuthManager) authorize(ctx context.Context, d *device) (credential, error) {
	interval := d.interval
	for {
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return credential{}, context.Cause(ctx)
		case <-timer.C:
		}
		ans, err := d.poll(ctx)
		switch {
		case err != nil:
			return credential{}, err
		case ans.credential != nil:
			return *ans.credential, nil
		case ans.slowDown && ans.interval > 0:
			interval = ans.interval
		case ans.slowDown:
			if interval > math.MaxInt64-5*m.unit {
				return credential{}, errorf(CodeProviderError, "device polling interval overflow")
			}
			interval += 5 * m.unit
		}
	}
}

func (m *AuthManager) finishLogin(ctx context.Context, l *login, cred credential, err error, issued bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if l.exchanging {
		l.uncertain = issued || (err != nil && uncertainAuthorizationError(err))
		if err == nil && cred.Access != "" && (!l.active() || ctx.Err() != nil) {
			l.uncertain = true
		}
		if a := m.accounts[l.status.Account]; a != nil && a.cred.PendingOperation != "" && err == nil && cred.Access != "" {
			copy := cred
			l.latest = &copy
		}
	}
	if ctx.Err() != nil {
		err = context.Cause(ctx)
	}
	state, code, msg := loginResult(l, err)
	rc := m.receipts[l.requestID]
	name, next, fresh := m.prepareLoginCredential(l, cred, &state, &code, &msg, &rc)
	rc.Status = l.status
	rc.Status.State, rc.Status.Code, rc.Status.Error = state, code, msg
	if err := m.commit(name, next, fresh, &rc); err != nil {
		state, code, msg = LoginFailed, CodeStorageError, err.Error()
	}
	l.end(state, code, msg)
}

func loginResult(l *login, err error) (string, ErrorCode, string) {
	switch {
	case !l.active():
		return l.status.State, l.status.Code, l.status.Error
	case errors.Is(err, errLoginExpired):
		return LoginExpired, CodeAuthRequired, "device code expired; start a new login"
	case errors.Is(err, context.Canceled):
		return LoginCancelled, "", "login cancelled"
	case err != nil:
		return LoginFailed, cmp.Or(ErrorCodeOf(err), CodeProviderError), loginMessage(err)
	default:
		return LoginCompleted, "", ""
	}
}

// prepareLoginCredential publishes accepted credentials, or records an
// exchange uncertainty without changing the current session.
func (m *AuthManager) prepareLoginCredential(l *login, cred credential, state *string, code *ErrorCode, msg *string, rc *authReceipt) (string, *credential, bool) {
	name, fresh := "", false
	var next *credential
	if *state == LoginCompleted {
		cred.Provider = l.status.Provider
		if err := m.validateActivation(l.status.Account, cred); err != nil {
			*state, *code, *msg = LoginFailed, CodeAuthRequired, err.Error()
		} else {
			now := time.Now().UTC()
			cred.Provider, cred.Generation, cred.Created, cred.Updated = l.status.Provider, newID("gen"), now, now
			cred.Status, cred.AuthorizationUncertain = "signed_in", l.priorAuthorizationUncertain
			if a := m.accounts[l.status.Account]; a != nil {
				cred.Plan = a.cred.Plan
				cred.Created = a.cred.Created
				cred.LastTransferID = a.cred.LastTransferID
				cred.LastTransferStatus = a.cred.LastTransferStatus
			}
			name, next, fresh = l.status.Account, &cred, true
			rc.Generation = cred.Generation
		}
	}
	if l.exchanging && next == nil {
		// A successful response that is rejected or discarded is still a
		// possible extra provider session, even if its token was known locally.
		if cred.Access != "" && *state != LoginCompleted {
			l.uncertain = true
		}
		if a := m.accounts[l.status.Account]; a != nil {
			c := a.cred
			c.AuthorizationUncertain = l.priorAuthorizationUncertain || l.uncertain
			name, next = l.status.Account, &c
		}
	}
	return name, next, fresh
}

func uncertainAuthorizationError(err error) bool {
	f := providerFailureOf(err)
	return f == nil || f.HTTPStatus < 300 || f.HTTPStatus >= 500
}

func loginMessage(err error) string {
	var e *Error
	var me *modelError
	switch {
	case errors.As(err, &e):
		return e.Message
	case errors.As(err, &me):
		return me.msg
	}
	return "login failed; try a new login"
}

// LoginStatus returns current or recovered status, including older logins.
func (m *AuthManager) LoginStatus(id string) (LoginStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.logins[id]
	if l == nil {
		return LoginStatus{}, errorf(CodeNotFound, "login not found")
	}
	return l.publicStatus(), nil
}

// Logins lists retained login statuses. Interrupted flows are cancelled on reopen.
func (m *AuthManager) Logins() []LoginStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []LoginStatus{}
	for _, l := range m.logins {
		out = append(out, l.publicStatus())
	}
	slices.SortFunc(out, func(a, b LoginStatus) int { return strings.Compare(a.ID, b.ID) })
	return out
}

// WaitLogin joins initial I/O and polling. Cancelling ctx stops only this wait.
func (m *AuthManager) WaitLogin(ctx context.Context, id string) (LoginStatus, error) {
	m.mu.Lock()
	l := m.logins[id]
	m.mu.Unlock()
	if l == nil {
		return LoginStatus{}, errorf(CodeNotFound, "login not found")
	}
	select {
	case <-l.stopped:
		return m.LoginStatus(id)
	case <-ctx.Done():
		return LoginStatus{}, ctx.Err()
	}
}

// CancelLogin durably records its acknowledgement, cancels and joins the login.
func (m *AuthManager) CancelLogin(req CancelLoginRequest) (LoginStatus, error) {
	m.mu.Lock()
	rc := authReceipt{RequestID: req.RequestID, Kind: "cancel", Target: req.LoginID}
	old, err := m.replay(rc)
	if old != nil || err != nil {
		m.mu.Unlock()
		if old != nil {
			return old.Ack, err
		}
		return LoginStatus{}, err
	}
	l := m.logins[req.LoginID]
	if l == nil {
		m.mu.Unlock()
		return LoginStatus{}, errorf(CodeNotFound, "login not found")
	}
	rc.Ack = l.status
	if l.active() {
		rc.Ack.State, rc.Ack.Code, rc.Ack.Error = LoginCancelled, "", "cancelled"
	}
	err = m.commit("", nil, false, &rc)
	if err == nil {
		l.end(LoginCancelled, "", "cancelled")
	}
	m.mu.Unlock()
	if err == nil {
		<-l.stopped
	}
	return rc.Ack, err
}
