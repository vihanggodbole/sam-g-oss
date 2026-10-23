package main

import (
	"context"
	"crypto/subtle"
	"net/http"
	"net/url"
	"strings"
	"time"

	sam "github.com/vihanggodbole/sam-g-oss"
)

func restoredModel(session sam.Session, settings sam.SessionSettings) sam.Model {
	if b := session.Binding; b != nil {
		m := chatModel(b.Provider, b.ModelID, 0)
		m.API = b.API
		if b.API == sam.APIAnthropicMessages {
			m.Unavailable = "Messages output bound is missing; restore models.json or configure verified max_output_tokens"
		}
		return m
	}
	alias := settings.Effective.Model
	// Earlier ChatGPT-only sessions used raw IDs and inherited their account.
	// Keep those entries unchanged; new sessions explicitly save the account
	// and use provider-qualified aliases, even before their first message.
	if provider, id, ok := strings.Cut(alias, "/"); ok && settings.Overrides.Account != nil && (provider == "chatgpt" || provider == "copilot") {
		return chatModel(provider, id, 0)
	}
	return chatModel("chatgpt", alias, 0)
}

func (a *app) checkAccount(provider, account string) error {
	p, ok := a.cfg.Providers[provider]
	if !ok {
		return problem(sam.CodeInvalidArgument, "Provider is not enabled in this host.")
	}
	if account == "" {
		return problem(sam.CodeInvalidArgument, "Select an explicit named account.")
	}
	for _, info := range a.auth.Accounts() {
		if info.Name == account && info.Provider != p.Auth {
			return problem(sam.CodeConflict, "This account belongs to another provider. Create a new session to change providers.")
		}
	}
	// Like the library, setup may precede login. Execution still requires
	// that exact account; there is never another-account/billing fallback.
	return nil
}

func (a *app) startBrowserLogin(ctx context.Context, c command) (any, error) {
	if err := a.checkAccount("chatgpt", c.Account); err != nil {
		return nil, err
	}
	st, err := a.auth.StartLogin(ctx, sam.LoginRequest{
		RequestID: c.RequestID, Provider: sam.AuthOpenAIChatGPT, Account: c.Account,
		RedirectURI: a.origin + "/auth/callback", EnablePlanUsage: true,
	})
	return st, err
}

func (a *app) callback(w http.ResponseWriter, r *http.Request) {
	states := r.URL.Query()["state"]
	id := ""
	if len(states) == 1 && states[0] != "" {
		// Correlate against the manager's live browser attempts, not one global
		// last-login ID. Different accounts/tabs can finish in either order.
		// AuthManager still owns all callback/state/nonce/PKCE validation.
		for _, login := range a.auth.Logins() {
			u, err := url.Parse(login.AuthorizationURL)
			if err == nil && login.Flow == "authorization_code" && subtle.ConstantTimeCompare([]byte(u.Query().Get("state")), []byte(states[0])) == 1 {
				id = login.ID
				break
			}
		}
	}
	if id == "" {
		http.Error(w, "No matching active sign-in. Return to the example page. Do not share this callback URL.", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	_, err := a.auth.CompleteLogin(ctx, sam.CompleteLoginRequest{LoginID: id, CallbackURL: a.origin + "/auth/callback?" + r.URL.RawQuery})
	if err != nil {
		http.Error(w, "Callback rejected. Return to the example page for status. Do not share this callback URL.", http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
