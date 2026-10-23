package sam

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"
)

// LogoutResult describes this accepted sign-out, not current account status.
// A later login is never affected by replaying its request ID.
type LogoutResult struct {
	LocalSignedOut   bool   `json:"local_signed_out"`
	RemoteRevocation string `json:"remote_revocation"` // confirmed, unconfirmed, not_applicable
	Warning          string `json:"warning,omitempty"`
}

func unconfirmedLogout() LogoutResult {
	return LogoutResult{LocalSignedOut: true, RemoteRevocation: "unconfirmed", Warning: "Signed out locally, but remote revocation was not confirmed. Disconnect the app in ChatGPT Settings: https://chatgpt.com/settings/usage"}
}

func (m *AuthManager) validateActivation(name string, c credential) error {
	if old := m.accounts[name]; old != nil {
		if old.cred.Provider != c.Provider {
			return errorf(CodeConflict, "saved account belongs to another provider; use a different label")
		}
		if old.cred.PendingOperation != "" {
			return errorf(CodeConflict, "account operation is still settling")
		}
		p := old.cred.Profile
		if p.ClientID != "" && p.ClientID != c.Profile.ClientID {
			return errorf(CodeConflict, "issued registration belongs to another client; use a different label")
		}
		if p.Subject != "" && !sameRegistration(p, c.Profile) {
			return errorf(CodeAuthRequired, "identity does not match the saved registration")
		}
	}
	if c.Provider == AuthGitHubCopilot {
		for other, a := range m.accounts {
			if other != name && a.cred.usable() && a.cred.GitHub == c.GitHub {
				return errorf(CodeConflict, "Copilot session already active under another label")
			}
		}
	}
	if c.Profile.Subject != "" {
		for other, a := range m.accounts {
			if other != name && (a.cred.usable() || a.cred.PendingOperation != "") && sameRegistration(a.cred.Profile, c.Profile) {
				return errorf(CodeConflict, "registration already active under account %q", other)
			}
		}
	}
	return nil
}

// Logout durably disables the selected generation before any remote call.
// Cancellation stops remote waiting, not accepted local clearing. The direct
// caller must supply a deadline if desired; attempt count is not a time bound.
func (m *AuthManager) Logout(ctx context.Context, req AuthAccountRequest) (LogoutResult, error) {
	if err := ctx.Err(); err != nil {
		return LogoutResult{}, err
	}
	m.mu.Lock()
	rc := authReceipt{RequestID: req.RequestID, Kind: "logout", Target: req.Account}
	old, err := m.replay(rc)
	if old != nil || err != nil {
		m.mu.Unlock()
		if err != nil {
			return LogoutResult{}, err
		}
		return m.waitLogout(ctx, req.RequestID)
	}
	a, waits, err := m.retireLocked(&rc, "signing_out")
	if err != nil {
		m.mu.Unlock()
		return LogoutResult{}, err
	}
	workCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(m.ctx, cancel)
	m.wg.Go(func() {
		defer cancel()
		defer stop()
		c, uncertain := m.settleRetirement(a, waits)
		result := LogoutResult{LocalSignedOut: true, RemoteRevocation: "not_applicable"}
		if c.Provider == AuthOpenAIChatGPT {
			result = unconfirmedLogout()
			if c.Refresh != "" && m.revoke(workCtx, c) && !uncertain {
				result.RemoteRevocation = "confirmed"
				result.Warning = ""
			}
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		c.Plan = a.cred.Plan
		c.clearTokens()
		rc.Pending = false
		rc.Logout = result
		_ = m.commit(rc.Target, &c, false, &rc)
		close(m.operations[rc.RequestID])
		delete(m.operations, rc.RequestID)
	})
	m.mu.Unlock()
	// Once accepted, finish local state even if the caller cancels. The network
	// observes ctx through workCtx; joining retains the latest known rotation.
	return m.waitLogout(context.Background(), req.RequestID)
}

func (m *AuthManager) waitLogout(ctx context.Context, requestID string) (LogoutResult, error) {
	if err := m.waitAuthOperation(ctx, requestID); err != nil {
		return LogoutResult{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.broken != nil {
		return LogoutResult{}, m.broken
	}
	return m.receipts[requestID].Logout, nil
}

func (m *AuthManager) waitAuthOperation(ctx context.Context, id string) error {
	m.mu.Lock()
	done := m.operations[id]
	m.mu.Unlock()
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// retireLocked commits unusability first, then cancels owned operations.
func (m *AuthManager) retireLocked(rc *authReceipt, status string) (*account, []*login, error) {
	a := m.accounts[rc.Target]
	var waits []*login
	for _, l := range m.logins {
		if l.status.Account == rc.Target && l.unsettled() {
			waits = append(waits, l)
		}
	}
	if a == nil {
		if len(waits) == 0 {
			return nil, nil, errorf(CodeNotFound, "account not found")
		}
		for _, l := range m.logins {
			if l.status.Account == rc.Target && l.unsettled() {
				a = &account{cred: credential{Provider: l.status.Provider, Generation: newID("gen")}}
				break
			}
		}
	}
	if a.cred.PendingOperation != "" {
		return nil, nil, errorf(CodeConflict, "account operation is still settling")
	}
	c := a.cred
	c.Status = status
	c.PendingOperation = rc.RequestID
	rc.Generation = c.Generation
	rc.Pending = true
	if err := m.commit(rc.Target, &c, false, rc); err != nil {
		return nil, nil, err
	}
	a = m.accounts[rc.Target]
	m.operations[rc.RequestID] = make(chan struct{})
	for _, l := range m.logins {
		if l.status.Account == rc.Target && l.active() {
			l.end(LoginCancelled, "", "cancelled by account operation")
		}
	}
	if a.refresh != nil {
		a.refresh.cancel()
	}
	return a, waits, nil
}

func (m *AuthManager) settleRetirement(a *account, waits []*login) (credential, bool) {
	for _, l := range waits {
		<-l.stopped
	}
	m.mu.Lock()
	w := a.refresh
	m.mu.Unlock()
	if w != nil {
		<-w.done
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	c := a.cred
	rotationUncertain := c.RotationUncertain
	authorizationUncertain := c.AuthorizationUncertain
	if w != nil {
		if w.latest != nil {
			// A known successful rotation resolves rotation uncertainty, but it
			// cannot resolve a separate authorization that may add a session.
			rotationUncertain = w.uncertain
			latest := *w.latest
			latest.AuthorizationUncertain = authorizationUncertain
			latest.Plan = c.Plan
			latest.Status = c.Status
			latest.PendingOperation = c.PendingOperation
			c = latest
		} else {
			rotationUncertain = rotationUncertain || w.uncertain
		}
		a.refresh = nil
	}
	for _, l := range waits {
		authorizationUncertain = authorizationUncertain || l.uncertain
		if l.latest != nil {
			// Revoke the newly known token, but do not claim that also revoked
			// the older or independently issued session.
			latest := *l.latest
			latest.Provider, latest.Generation, latest.Created = c.Provider, c.Generation, c.Created
			latest.Plan, latest.Status, latest.PendingOperation = c.Plan, c.Status, c.PendingOperation
			latest.RotationUncertain = rotationUncertain
			latest.AuthorizationUncertain = true
			c = latest
			authorizationUncertain = true
		}
	}
	c.RotationUncertain = rotationUncertain
	c.AuthorizationUncertain = authorizationUncertain
	uncertain := rotationUncertain || authorizationUncertain
	return c, uncertain
}

func (m *AuthManager) revoke(ctx context.Context, c credential) bool {
	for attempt := 0; attempt < 3; attempt++ {
		if ctx.Err() != nil {
			return false
		}
		r, err := m.send(ctx, http.MethodPost, openaiRevokeEndpoint, formType, url.Values{"token": {c.Refresh}, "token_type_hint": {"refresh_token"}, "client_id": {c.Profile.ClientID}}.Encode(), nil)
		if err == nil && r.status == http.StatusOK {
			return true
		}
		if err == nil && r.status != 429 && r.status < 500 {
			return false
		}
		if attempt == 2 {
			return false
		}
		delay := time.Duration(attempt+1) * time.Second
		if r.retryAfterKnown {
			delay = r.retryAfter
		}
		if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= delay {
			return false
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
	}
	return false
}

// AllowPlanUsageRetry acknowledges a user's review of Usage settings. It only
// clears this local block: no consent grant, provider request or turn start.
func (m *AuthManager) AllowPlanUsageRetry(ctx context.Context, req AuthAccountRequest) (AccountInfo, error) {
	if err := ctx.Err(); err != nil {
		return AccountInfo{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	rc := authReceipt{RequestID: req.RequestID, Kind: "retry_plan", Target: req.Account}
	old, err := m.replay(rc)
	if old != nil {
		return old.Account, err
	}
	if err != nil {
		return AccountInfo{}, err
	}
	a := m.accounts[req.Account]
	if a == nil {
		return AccountInfo{}, errorf(CodeNotFound, "account not found")
	}
	c := a.cred
	c.Plan = planState{}
	rc.Generation = c.Generation
	rc.Account = accountInfo(req.Account, c)
	if err := m.commit(req.Account, &c, false, &rc); err != nil {
		return AccountInfo{}, err
	}
	return rc.Account, nil
}

// admitPlan is the synchronous boundary immediately before each inference HTTP
// attempt. Catalog reads intentionally bypass this spending gate.
func (m *AuthManager) admitPlan(ref accountRef) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, c, err := m.currentLocked(ref)
	if err != nil {
		return err
	}
	if !c.planEnabled() {
		return errorf(CodeAuthRequired, "ChatGPT plan use is disabled; explicitly authorize with EnablePlanUsage or choose API-key billing separately")
	}
	if c.Plan.Blocked {
		return &Error{Code: CodeProviderError, Message: "ChatGPT plan requests are blocked locally. Review https://chatgpt.com/settings/usage, then explicitly allow plan usage retry", ProviderFailure: accountInfo(ref.name, c).PlanFailure}
	}
	return nil
}

func (m *AuthManager) recordPlanFailure(ref accountRef, err error) error {
	var e *modelError
	if !errors.As(err, &e) || e.failure == nil || e.failure.Code != "subscription_sharing_usage_limit_exceeded" {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	a := m.accounts[ref.name]
	// A response admitted before reauthorization may still report this same
	// registration's limit. Login generations pin inference identity, not a
	// quota reset. Verified registration identity cannot be replaced in place.
	if a == nil || ref.clientID == "" || a.cred.Profile.ClientID != ref.clientID || a.cred.Provider != AuthOpenAIChatGPT {
		return err
	}
	c := a.cred
	now := time.Now().UTC()
	c.Plan = planState{Blocked: true, ObservedAt: &now, Failure: e.failure, ResetAt: e.failure.ResetAt}
	if saveErr := m.commit(ref.name, &c, false, nil); saveErr != nil {
		return saveErr
	}
	return err
}
