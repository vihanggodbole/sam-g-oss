package sam

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// ExportAccountRequest moves one token session into a protected local bundle.
// The source is disabled before file I/O and never remotely revoked.
type ExportAccountRequest struct {
	RequestID string `json:"request_id"`
	Account   string `json:"account"`
	Path      string `json:"path"`
}

// ImportAccountRequest trusts a supported protected bundle from the user's own
// machine. It cannot prove ownership of arbitrary opaque bearer token pairs.
type ImportAccountRequest struct {
	RequestID string `json:"request_id"`
	Account   string `json:"account"`
	Path      string `json:"path"`
}

// TransferResult is safe to display. After an interrupted export, confirm source
// deactivation and inspect the bundle before importing, or sign in afresh.
type TransferResult struct {
	TransferID        string `json:"transfer_id"`
	Path              string `json:"path"`
	SourceDeactivated bool   `json:"source_deactivated"`
	Confirmed         bool   `json:"confirmed"`
	Warning           string `json:"warning,omitempty"`
}
type accountBundle struct {
	Format     string     `json:"format"`
	TransferID string     `json:"transfer_id"`
	Account    credential `json:"account"`
}

// ExportAccount performs a move, not a backup/copy. An accepted export always
// settles source deactivation, including on cancellation or manager shutdown.
func (m *AuthManager) ExportAccount(ctx context.Context, req ExportAccountRequest) (TransferResult, error) {
	if err := ctx.Err(); err != nil {
		return TransferResult{}, err
	}
	path, err := transferPath(req.Path)
	if err != nil {
		return TransferResult{}, err
	}
	m.mu.Lock()
	rc := authReceipt{RequestID: req.RequestID, Kind: "export", Target: req.Account, Identity: authIdentity(path)}
	old, err := m.replay(rc)
	if old != nil || err != nil {
		m.mu.Unlock()
		if err != nil {
			return TransferResult{}, err
		}
		return m.waitTransfer(ctx, *old)
	}
	a := m.accounts[req.Account]
	if a == nil || !a.cred.usable() {
		m.mu.Unlock()
		return TransferResult{}, errorf(CodeAuthRequired, "export requires a signed-in account")
	}
	rc.Transfer = TransferResult{TransferID: newID("transfer"), Path: path}
	a, waits, err := m.retireLocked(&rc, "exporting")
	if err != nil {
		m.mu.Unlock()
		return TransferResult{}, err
	}
	m.wg.Go(func() { m.runExport(ctx, a, waits, rc) })
	m.mu.Unlock()
	return m.waitTransfer(context.Background(), rc)
}

func transferPath(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errorf(CodeInvalidArgument, "bundle path is required")
	}
	p, err := filepath.Abs(path)
	if err != nil {
		return "", errorf(CodeInvalidArgument, "invalid bundle path")
	}
	return p, nil
}

func (m *AuthManager) runExport(ctx context.Context, a *account, waits []*login, rc authReceipt) {
	c, _ := m.settleRetirement(a, waits)
	err := ctx.Err()
	if err == nil {
		err = m.ctx.Err()
	}
	if err == nil && c.RotationUncertain {
		err = errorf(CodeAuthRequired, "refresh rotation outcome is uncertain; no reliable session can be exported")
	}
	if err == nil {
		if _, statErr := os.Lstat(rc.Transfer.Path); !os.IsNotExist(statErr) {
			err = errorf(CodeConflict, "bundle destination must not already exist")
		}
	}
	if err == nil {
		bundleCred := c
		bundleCred.Status = "signed_in"
		bundleCred.PendingOperation = ""
		bundle := accountBundle{Format: "sam-g-account-v1", TransferID: rc.Transfer.TransferID, Account: bundleCred}
		err = writePrivate(rc.Transfer.Path, []byte(mustJSON(bundle)))
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// Include limits learned by already-admitted requests while moving.
	c.Plan = a.cred.Plan
	c.clearTokens()
	c.LastTransferID = rc.Transfer.TransferID
	c.LastTransferStatus = "exported"
	if err != nil {
		c.LastTransferStatus = "export_unconfirmed"
	}
	rc.Pending = false
	rc.Transfer.SourceDeactivated = true
	rc.Transfer.Confirmed = err == nil
	if c.RotationUncertain {
		rc.Transfer.Warning = "Source is deactivated, but refresh rotation is uncertain; no bundle was made. Sign in again before retrying."
	} else if err != nil {
		rc.Transfer.Warning = "Source deactivated, but export was not confirmed. Inspect a complete bundle before use, or sign in again."
		if c.AuthorizationUncertain {
			rc.Transfer.Warning += " Additional remote sessions may remain."
		}
	} else if c.AuthorizationUncertain {
		rc.Transfer.Warning = "The known session was moved, but additional remote sessions may remain. Review app connections in ChatGPT Settings if you want to disconnect them."
	}
	_ = m.commit(rc.Target, &c, false, &rc)
	close(m.operations[rc.RequestID])
	delete(m.operations, rc.RequestID)
}

func (m *AuthManager) waitTransfer(ctx context.Context, rc authReceipt) (TransferResult, error) {
	if err := m.waitAuthOperation(ctx, rc.RequestID); err != nil {
		return TransferResult{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.broken != nil {
		return TransferResult{}, m.broken
	}
	return m.receipts[rc.RequestID].Transfer, nil
}

// ImportAccount atomically saves the registration, credentials and transfer-ID
// receipt. Reimport can never overwrite rotated destination credentials.
func (m *AuthManager) ImportAccount(ctx context.Context, req ImportAccountRequest) (AccountInfo, error) {
	if err := ctx.Err(); err != nil {
		return AccountInfo{}, err
	}
	path, err := transferPath(req.Path)
	if err != nil {
		return AccountInfo{}, err
	}
	rc := authReceipt{RequestID: req.RequestID, Kind: "import", Target: req.Account, Identity: authIdentity(path)}
	m.mu.Lock()
	old, err := m.replay(rc)
	m.mu.Unlock()
	if old != nil {
		return old.Account, err
	}
	if err != nil {
		return AccountInfo{}, err
	}
	bundle, err := readAccountBundle(path)
	if err != nil {
		return AccountInfo{}, err
	}
	if err = m.validateBundle(ctx, bundle); err != nil {
		return AccountInfo{}, err
	}
	if err = ctx.Err(); err != nil {
		return AccountInfo{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	old, err = m.replay(rc)
	if old != nil {
		return old.Account, err
	}
	if err != nil {
		return AccountInfo{}, err
	}
	if !validAccountName(req.Account) {
		return AccountInfo{}, errorf(CodeInvalidArgument, "invalid account label")
	}
	for _, previous := range m.receipts {
		if previous.Kind == "import" && previous.Account.LastTransferID == bundle.TransferID {
			return AccountInfo{}, errorf(CodeConflict, "transfer already imported into %q; inspect Accounts instead of overwriting credentials", previous.Target)
		}
	}
	if a := m.accounts[req.Account]; a != nil && a.cred.usable() {
		return AccountInfo{}, errorf(CodeConflict, "destination account is already active")
	}
	for _, l := range m.logins {
		if l.status.Account == req.Account && l.unsettled() {
			return AccountInfo{}, errorf(CodeConflict, "destination login is still settling")
		}
	}
	c := bundle.Account
	if err = m.validateActivation(req.Account, c); err != nil {
		return AccountInfo{}, err
	}
	if a := m.accounts[req.Account]; a != nil {
		c.AuthorizationUncertain = c.AuthorizationUncertain || a.cred.AuthorizationUncertain
		if a.cred.Plan.Blocked {
			c.Plan = a.cred.Plan
		}
	}
	c.Generation = newID("gen")
	c.Status = "signed_in"
	c.PendingOperation = ""
	c.LastTransferID = bundle.TransferID
	c.LastTransferStatus = "imported"
	c.Updated = time.Now().UTC()
	rc.Generation = c.Generation
	rc.Account = accountInfo(req.Account, c)
	if err = m.commit(req.Account, &c, true, &rc); err != nil {
		return AccountInfo{}, err
	}
	return rc.Account, nil
}

func readAccountBundle(path string) (accountBundle, error) {
	var b accountBundle
	if err := privateDirectory(filepath.Dir(path)); err != nil {
		return b, err
	}
	if err := privateFile(path); err != nil {
		return b, err
	}
	// This is an explicitly trusted local file, not a remote response body.
	// Its retained ID token and latest token response may together exceed the
	// per-response auth bound; do not reject our own exported registration.
	raw, err := os.ReadFile(path)
	if err != nil {
		return b, errorf(CodeInvalidArgument, "cannot read protected account bundle")
	}
	if json.Unmarshal(raw, &b) != nil || b.Format != "sam-g-account-v1" || !strings.HasPrefix(b.TransferID, "transfer_") {
		return b, errorf(CodeInvalidArgument, "invalid sam-g account bundle")
	}

	return b, nil
}

func (m *AuthManager) validateBundle(ctx context.Context, b accountBundle) error {
	c := b.Account
	if !validAuth(c.Provider) || !c.usable() || c.Generation == "" || c.RotationUncertain {
		return errorf(CodeInvalidArgument, "bundle has no usable credential session")
	}
	if c.Provider == AuthGitHubCopilot {
		if c.GitHub == "" {
			return errorf(CodeInvalidArgument, "bundle lacks GitHub authorization")
		}
		_, err := copilotEndpoint(c.Endpoint)
		return err
	}
	if c.Expires.IsZero() || !slices.Contains(c.Scopes, "openid") || (slices.Contains(c.Scopes, "offline_access") && c.Refresh == "") {
		return errorf(CodeInvalidArgument, "bundle token metadata is incomplete")
	}
	if c.IDToken == "" || !strings.EqualFold(c.TokenType, "Bearer") || c.Profile.RedirectScheme != "http" || c.Profile.RedirectHost != "127.0.0.1" || c.Profile.RedirectPath != "/auth/callback" {
		return errorf(CodeInvalidArgument, "bundle registration is malformed")
	}
	profile, err := m.validateID(ctx, c.IDToken, c.Profile.ClientID, "", true)
	if err != nil {
		return err
	}
	if !sameRegistration(profile, c.Profile) {
		return errorf(CodeInvalidArgument, "bundle ID token does not match its registration")
	}
	// Historical signature/identity consistency is not fresh login proof.
	return nil
}
