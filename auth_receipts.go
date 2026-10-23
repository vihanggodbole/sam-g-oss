package sam

import (
	"crypto/sha256"
	"encoding/hex"
)

func authIdentity(v any) string {
	sum := sha256.Sum256([]byte(mustJSON(v)))
	return hex.EncodeToString(sum[:])
}

// AuthAccountRequest identifies a durable account mutation. Replaying it returns
// that operation's outcome, not the current state of a later login or limit.
type AuthAccountRequest struct {
	RequestID string `json:"request_id"`
	Account   string `json:"account"`
}

// CancelLoginRequest cancels a login. Identical retries return the original
// status acknowledgement, not the state of a later login.
type CancelLoginRequest struct {
	RequestID string `json:"request_id"`
	LoginID   string `json:"login_id"`
}

// authReceipt holds only non-secret mutation identity and acknowledgements.
// Start's Ack is fixed once the initial request settles; Status follows that
// login independently. Receipts are retained, including after replacement.
type authReceipt struct {
	RequestID  string         `json:"request_id"`
	Kind       string         `json:"kind"`
	Target     string         `json:"target"`
	Provider   string         `json:"provider,omitempty"`
	Ack        LoginStatus    `json:"ack,omitempty"`
	Status     LoginStatus    `json:"status,omitempty"`
	Failure    *Error         `json:"failure,omitempty"`
	Identity   string         `json:"identity,omitempty"`
	Generation string         `json:"generation,omitempty"`
	Pending    bool           `json:"pending,omitempty"`
	Logout     LogoutResult   `json:"logout,omitempty"`
	Transfer   TransferResult `json:"transfer,omitempty"`
	Account    AccountInfo    `json:"account,omitempty"`
}

// replay checks identity before mutable provider/account state. Caller holds mu.
func (m *AuthManager) replay(want authReceipt) (*authReceipt, error) {
	if m.closed {
		return nil, errAuthClosed
	}
	if m.broken != nil {
		return nil, m.broken
	}
	if want.RequestID == "" {
		return nil, errorf(CodeInvalidArgument, "request_id is required")
	}
	old, ok := m.receipts[want.RequestID]
	if !ok {
		return nil, nil
	}
	if old.Kind != want.Kind || old.Target != want.Target || old.Provider != want.Provider || old.Identity != want.Identity {
		return nil, errorf(CodeConflict, "request_id was already used with different arguments")
	}
	if old.Failure != nil {
		e := *old.Failure
		return &old, &e
	}
	return &old, nil
}

// recoverLogins never resumes network work. A reservation persisted before the
// initial request is also a receipt: replay cannot start it again after a crash.
func (m *AuthManager) recoverLogins() error {
	for id, rc := range m.receipts {
		if rc.Kind != "start" {
			continue
		}
		if rc.Status.State == LoginStarting || rc.Status.State == LoginPending {
			rc.Status.State, rc.Status.Error = LoginCancelled, "login interrupted by auth manager shutdown; start a new login with a new request_id"
			if rc.Ack.State == LoginStarting || rc.Status.Flow == "authorization_code" {
				rc.Ack = rc.Status
			}
			if err := m.commit("", nil, false, &rc); err != nil {
				return err
			}
		}
		l := &login{status: rc.Status, requestID: id, ready: make(chan struct{}), stopped: make(chan struct{}), cancel: func() {}}
		close(l.ready)
		close(l.stopped)
		m.logins[l.status.ID] = l
	}
	return nil
}
