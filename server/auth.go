package server

import (
	"context"
	"net/http"
	"time"

	sam "github.com/vihanggodbole/sam-g-oss"
)

// authRoutes exposes the runtime's AuthManager: device-code logins and
// account listing/logout. Login runs on this host; the response carries
// the provider's verification URL and user code for the user to open in
// any browser. Closing the HTTP request does not cancel a started login.
func (s *Server) authRoutes() {
	m := s.mux
	m.HandleFunc("GET /v1/auth/accounts", api(s.withAuth(func(a *sam.AuthManager, _ *http.Request) (int, any, error) {
		return http.StatusOK, map[string]any{"accounts": a.Accounts()}, nil
	})))
	m.HandleFunc("DELETE /v1/auth/accounts/{name}", api(s.withAuth(func(a *sam.AuthManager, r *http.Request) (int, any, error) {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		out, err := a.Logout(ctx, sam.AuthAccountRequest{RequestID: requestID(r), Account: r.PathValue("name")})
		return http.StatusOK, out, err
	})))
	m.HandleFunc("POST /v1/auth/logins", api(s.withAuth(func(a *sam.AuthManager, r *http.Request) (int, any, error) {
		var body struct {
			Provider        string `json:"provider"`
			Account         string `json:"account"`
			RedirectURI     string `json:"redirect_uri,omitempty"`
			EnablePlanUsage bool   `json:"enable_plan_usage,omitempty"`
		}
		if err := s.decode(r, &body, false); err != nil {
			return 0, nil, err
		}
		out, err := a.StartLogin(r.Context(), sam.LoginRequest{RequestID: requestID(r), Provider: body.Provider, Account: body.Account, RedirectURI: body.RedirectURI, EnablePlanUsage: body.EnablePlanUsage})
		return http.StatusAccepted, out, err
	})))
	m.HandleFunc("POST /v1/auth/accounts/{name}/retry-plan-usage", api(s.withAuth(func(a *sam.AuthManager, r *http.Request) (int, any, error) {
		out, err := a.AllowPlanUsageRetry(r.Context(), sam.AuthAccountRequest{RequestID: requestID(r), Account: r.PathValue("name")})
		return http.StatusOK, out, err
	})))
	m.HandleFunc("POST /v1/auth/logins/{id}/complete", api(s.withAuth(func(a *sam.AuthManager, r *http.Request) (int, any, error) {
		var body struct {
			CallbackURL string `json:"callback_url"`
		}
		if err := s.decode(r, &body, false); err != nil {
			return 0, nil, err
		}
		out, err := a.CompleteLogin(r.Context(), sam.CompleteLoginRequest{LoginID: r.PathValue("id"), CallbackURL: body.CallbackURL})
		return http.StatusOK, out, err
	})))
	m.HandleFunc("GET /v1/auth/logins", api(s.withAuth(func(a *sam.AuthManager, _ *http.Request) (int, any, error) {
		return http.StatusOK, map[string]any{"logins": a.Logins()}, nil
	})))
	m.HandleFunc("GET /v1/auth/logins/{id}", api(s.withAuth(func(a *sam.AuthManager, r *http.Request) (int, any, error) {
		out, err := a.LoginStatus(r.PathValue("id"))
		return http.StatusOK, out, err
	})))
	m.HandleFunc("DELETE /v1/auth/logins/{id}", api(s.withAuth(func(a *sam.AuthManager, r *http.Request) (int, any, error) {
		out, err := a.CancelLogin(sam.CancelLoginRequest{RequestID: requestID(r), LoginID: r.PathValue("id")})
		return http.StatusOK, out, err
	})))
}

// withAuth fails clearly when the host configured no subscription login.
func (s *Server) withAuth(fn func(*sam.AuthManager, *http.Request) (int, any, error)) apiFunc {
	return func(r *http.Request) (int, any, error) {
		a := s.rt.Auth()
		if a == nil {
			return 0, nil, &sam.Error{Code: sam.CodeUnsupported, Message: "subscription login is not configured on this server (no auth section)"}
		}
		return fn(a, r)
	}
}
