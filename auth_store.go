package sam

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const authSchema = `
CREATE TABLE IF NOT EXISTS auth_meta (version INTEGER NOT NULL, host_id TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS accounts (
 label TEXT PRIMARY KEY, provider TEXT NOT NULL, generation TEXT NOT NULL,
 status TEXT NOT NULL, profile TEXT NOT NULL, credential TEXT NOT NULL, plan TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS auth_requests (
 request_id TEXT PRIMARY KEY, kind TEXT NOT NULL, target TEXT NOT NULL,
 generation TEXT NOT NULL, pending INTEGER NOT NULL, result TEXT NOT NULL
);`

// OpenAuth exclusively owns cfg.DatabasePath. No provider request is made.
// Interrupted browser attempts are cancelled; accepted retirements finish
// locally, without a remote retry queue or automatic source reactivation.
func OpenAuth(cfg AuthConfig) (*AuthManager, error) {
	if err := cfg.normalize(); err != nil {
		return nil, err
	}
	path, err := filepath.Abs(cfg.DatabasePath)
	if err != nil {
		return nil, errorf(CodeInvalidArgument, "auth database_path is invalid")
	}
	if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, storageErr(err)
	}
	if err = privateDirectory(filepath.Dir(path)); err != nil {
		return nil, err
	}
	path, lock, err := lockOwned(path, "auth database")
	if err != nil {
		return nil, storageErr(err)
	}
	if err = privateAuthFiles(path); err != nil {
		_ = lock.Close()
		return nil, err
	}
	db, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		_ = lock.Close()
		return nil, storageErr(err)
	}
	db.SetMaxOpenConns(1)
	m := &AuthManager{
		cfg: cfg, path: path, lock: lock, db: db, hc: noRedirectClient(cfg.HTTPClient), unit: time.Second,
		accounts: map[string]*account{}, logins: map[string]*login{}, receipts: map[string]authReceipt{}, operations: map[string]chan struct{}{},
	}
	m.ctx, m.cancel = context.WithCancel(context.Background())
	if err = m.loadAuth(); err == nil {
		err = privateAuthFiles(path)
	}
	if err == nil {
		err = m.recoverAuth()
	}
	if err != nil {
		m.cancel()
		_ = db.Close()
		_ = lock.Close()
		return nil, err
	}
	return m, nil
}

func privateDirectory(path string) error {
	st, err := os.Stat(path)
	if err != nil || !st.IsDir() || st.Mode().Perm()&0o077 != 0 {
		return errorf(CodeInvalidArgument, "auth directory must be owner-only (chmod 700): %s", path)
	}
	return nil
}

func privateFile(path string) error {
	st, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return storageErr(err)
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0o077 != 0 {
		return errorf(CodeInvalidArgument, "secret file must be a regular owner-only file (chmod 600): %s", path)
	}
	return nil
}

func privateAuthFiles(path string) error {
	for _, suffix := range []string{"", "-wal", "-shm", ".lock"} {
		if err := privateFile(path + suffix); err != nil {
			return err
		}
	}
	return nil
}

func newHostID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("urn:uuid:%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func (m *AuthManager) loadAuth() error {
	tx, err := m.db.BeginTx(bg, nil)
	if err != nil {
		return storageErr(err)
	}
	defer tx.Rollback()
	var unrelated int
	if err := tx.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name NOT IN ('auth_meta','accounts','auth_requests') AND name NOT LIKE 'sqlite_%'`).Scan(&unrelated); err != nil {
		return storageErr(err)
	}
	if unrelated != 0 {
		return errorf(CodeInvalidArgument, "auth database_path names a different application's or conversation database; choose a separate auth SQLite file")
	}
	if _, err = tx.Exec(authSchema); err != nil {
		return storageErr(err)
	}
	var version int
	err = tx.QueryRow("SELECT version,host_id FROM auth_meta").Scan(&version, &m.hostID)
	if err == sql.ErrNoRows {
		m.hostID = newHostID()
		_, err = tx.Exec("INSERT INTO auth_meta VALUES (1,?)", m.hostID)
	} else if err == nil && version != 1 {
		return errorf(CodeInvalidArgument, "unsupported auth database version")
	}
	if err != nil {
		return storageErr(err)
	}
	err = queryRows(tx, func(r *sql.Rows) error {
		var label, provider, generation, status, profile, raw, plan string
		if err := r.Scan(&label, &provider, &generation, &status, &profile, &raw, &plan); err != nil {
			return err
		}
		var c credential
		if err := json.Unmarshal([]byte(raw), &c); err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(profile), &c.Profile); err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(plan), &c.Plan); err != nil {
			return err
		}
		c.Provider, c.Generation, c.Status = provider, generation, status
		if !validAccountName(label) || !validAuth(provider) {
			return errorf(CodeInvalidArgument, "invalid auth account record")
		}
		m.accounts[label] = &account{cred: c}
		return nil
	}, "SELECT label,provider,generation,status,profile,credential,plan FROM accounts")
	if err != nil {
		return storageErr(err)
	}
	err = queryRows(tx, func(r *sql.Rows) error {
		var raw string
		if err := r.Scan(&raw); err != nil {
			return err
		}
		var rc authReceipt
		if err := json.Unmarshal([]byte(raw), &rc); err != nil {
			return err
		}
		m.receipts[rc.RequestID] = rc
		return nil
	}, "SELECT result FROM auth_requests")
	if err != nil {
		return storageErr(err)
	}
	return storageErr(tx.Commit())
}

// commit changes one account and receipt in one transaction. Caller holds mu.
// Empty name means no account mutation; nil credential deletes that label.
func (m *AuthManager) commit(name string, c *credential, fresh bool, rc *authReceipt) error {
	if m.broken != nil {
		return m.broken
	}
	tx, err := m.db.BeginTx(bg, nil)
	if err == nil {
		defer tx.Rollback()
		if name != "" {
			err = saveAuthAccount(tx, name, c)
		}
		if err == nil && rc != nil {
			err = saveAuthReceipt(tx, *rc)
		}
		if err == nil {
			err = tx.Commit()
		}
	}
	if err != nil {
		m.broken = errorf(CodeStorageError, "saving auth state failed; credential outcome is uncertain. Close and reopen the manager: %v", err)
		m.cancel()
		return m.broken
	}
	if rc != nil {
		m.receipts[rc.RequestID] = *rc
	}
	if name != "" {
		switch a := m.accounts[name]; {
		case c == nil:
			delete(m.accounts, name)
		case fresh || a == nil:
			m.accounts[name] = &account{cred: *c}
		default:
			a.cred = *c
		}
	}
	return nil
}

func saveAuthAccount(tx *sql.Tx, name string, c *credential) error {
	if c == nil {
		_, err := tx.Exec("DELETE FROM accounts WHERE label=?", name)
		return err
	}
	private := *c
	private.Profile = registration{}
	private.Plan = planState{}
	_, err := tx.Exec(`INSERT INTO accounts VALUES (?,?,?,?,?,?,?) ON CONFLICT(label) DO UPDATE SET
 provider=excluded.provider,generation=excluded.generation,status=excluded.status,
 profile=excluded.profile,credential=excluded.credential,plan=excluded.plan`, name, c.Provider, c.Generation, c.Status, mustJSON(c.Profile), mustJSON(private), mustJSON(c.Plan))
	return err
}

func saveAuthReceipt(tx *sql.Tx, rc authReceipt) error {
	rc.Ack = durableLoginStatus(rc.Ack)
	rc.Status = durableLoginStatus(rc.Status)
	_, err := tx.Exec(`INSERT INTO auth_requests VALUES (?,?,?,?,?,?) ON CONFLICT(request_id) DO UPDATE SET
 kind=excluded.kind,target=excluded.target,generation=excluded.generation,pending=excluded.pending,result=excluded.result`, rc.RequestID, rc.Kind, rc.Target, rc.Generation, rc.Pending, mustJSON(rc))
	return err
}

func (m *AuthManager) recoverAuth() error {
	for id, rc := range m.receipts {
		if !rc.Pending || (rc.Kind != "logout" && rc.Kind != "export") {
			continue
		}
		if a := m.accounts[rc.Target]; a != nil && a.cred.Generation == rc.Generation {
			c := a.cred
			c.clearTokens()
			rc.Pending = false
			if rc.Kind == "logout" {
				rc.Logout = unconfirmedLogout()
			} else {
				c.LastTransferID = rc.Transfer.TransferID
				c.LastTransferStatus = "export_unconfirmed"
				rc.Transfer.SourceDeactivated = true
				rc.Transfer.Confirmed = false
				rc.Transfer.Warning = "Export was interrupted. Source is deactivated; use a complete protected bundle only after checking this status. Otherwise sign in again."
			}
			if err := m.commit(rc.Target, &c, false, &rc); err != nil {
				return err
			}
		} else {
			rc.Pending = false
			if err := m.commit("", nil, false, &rc); err != nil {
				return err
			}
		}
		m.receipts[id] = rc
	}
	return m.recoverLogins()
}

// writePrivate is used only for protected transfer files, never as
// the credential-store authority. Logical deletion is not forensic erasure.
func writePrivate(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := privateDirectory(dir); err != nil {
		return err
	}
	if err := privateFile(path); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".sam-auth-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if e := f.Close(); err == nil {
		err = e
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		return err
	}
	return syncCredentialDir(dir)
}

func syncCredentialDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
