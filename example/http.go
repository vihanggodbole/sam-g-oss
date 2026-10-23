package main

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	sam "github.com/vihanggodbole/sam-g-oss"
	"github.com/vihanggodbole/sam-g-oss/server"
)

//go:embed index.html app.js accounts.js live.js settings.js inspector.js style.css
var assets embed.FS

var page = template.Must(template.ParseFS(assets, "index.html"))

type command struct {
	RequestID   string     `json:"request_id"`
	Model       string     `json:"model"`
	Provider    string     `json:"provider"`
	Account     string     `json:"account"`
	Definition  *sam.Model `json:"model_definition,omitempty"`
	Acknowledge bool       `json:"acknowledge_repeated_effects"`
}

func (a *app) handler() http.Handler {
	api := server.New(a.rt)
	api.MaxBodyBytes = 64 << 10 // bounded chat/setup commands; tool output has its own runtime limit
	// Also establishes the fixed authority when a host invokes ServeHTTP
	// without net/http's listener context (as the offline tests do).
	if err := api.SetAllowedOrigins([]string{a.origin}); err != nil {
		panic(err) // origin was constructed from our bound TCP listener
	}
	mux := http.NewServeMux()
	mux.Handle("/v1/", api)
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = page.Execute(w, a.csrf)
	})
	for path, kind := range map[string]string{"app.js": "text/javascript", "accounts.js": "text/javascript", "live.js": "text/javascript", "settings.js": "text/javascript", "inspector.js": "text/javascript", "style.css": "text/css"} {
		mux.HandleFunc("GET /"+path, func(w http.ResponseWriter, _ *http.Request) {
			b, _ := assets.ReadFile(path)
			w.Header().Set("Content-Type", kind+"; charset=utf-8")
			_, _ = w.Write(b)
		})
	}
	mux.HandleFunc("GET /auth/callback", a.callback)
	mux.HandleFunc("GET /example/state", a.hostAPI(a.state))
	mux.HandleFunc("GET /example/events", a.hostAPI(a.eventPage))
	mux.HandleFunc("POST /example/{action}", a.hostAPI(a.prepare))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; object-src 'none'; form-action 'self'")
		if r.Host != strings.TrimPrefix(a.origin, "http://") {
			http.Error(w, "Open the exact 127.0.0.1 URL printed by the server.", http.StatusMisdirectedRequest)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// Only host-owned setup lives here: choosing model configuration and binding
// browser authorization to our callback. Chat, history, SSE, discovery, stop,
// logout and account status use the ordinary /v1 server API, without stripping
// any browser headers or maintaining a second execution/auth implementation.
func (a *app) hostAPI(fn func(context.Context, *http.Request) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		origin, site := r.Header.Get("Origin"), r.Header.Get("Sec-Fetch-Site")
		if (origin != "" && origin != a.origin) || (site != "" && site != "same-origin" && site != "none") ||
			subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Example-CSRF")), []byte(a.csrf)) != 1 {
			http.Error(w, "Reload the example page before trying again.", http.StatusForbidden)
			return
		}
		if r.Method == http.MethodPost {
			kind, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if kind != "application/json" {
				http.Error(w, "Expected application/json.", http.StatusUnsupportedMediaType)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		out, err := fn(ctx, r)
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			var e *sam.Error
			if !errors.As(err, &e) {
				e = &sam.Error{Code: sam.CodeProviderError, Message: "Setup did not finish. Check the current chat before retrying."}
			}
			status := http.StatusBadGateway
			switch e.Code {
			case sam.CodeInvalidArgument:
				status = http.StatusBadRequest
			case sam.CodeNotFound:
				status = http.StatusNotFound
			case sam.CodeConflict, sam.CodeAuthRequired, sam.CodeUnsupported:
				status = http.StatusConflict
			case sam.CodeStorageError, sam.CodeClosed:
				status = http.StatusServiceUnavailable
			}
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": e})
			return
		}
		_ = json.NewEncoder(w).Encode(out)
	}
}

func problem(code sam.ErrorCode, message string) error {
	return &sam.Error{Code: code, Message: message}
}

func (a *app) prepare(ctx context.Context, r *http.Request) (any, error) {
	var c command
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil || d.Decode(new(any)) != io.EOF || c.RequestID == "" || len(c.RequestID) > 128 {
		return nil, problem(sam.CodeInvalidArgument, "Expected one JSON setup command with a request_id.")
	}
	switch r.PathValue("action") {
	case "login":
		return a.startBrowserLogin(ctx, c)
	case "model":
		return a.configureModel(ctx, c)
	case "resume":
		if !c.Acknowledge {
			return nil, problem(sam.CodeInvalidArgument, sam.DuplicateEffectWarning)
		}
		if err := a.rt.Resume(ctx); err != nil {
			return nil, err
		}
		return map[string]string{"warning": sam.DuplicateEffectWarning}, nil
	default:
		return nil, problem(sam.CodeNotFound, "Unknown setup action.")
	}
}

func (a *app) state(_ context.Context, _ *http.Request) (any, error) {
	_, copilot := a.cfg.Providers["copilot"]
	return map[string]any{"default_workspace": a.workspace, "copilot_enabled": copilot, "resume_warning": sam.DuplicateEffectWarning}, nil
}

// A paged read of the same durable events used by /v1/.../events SSE. This is
// an inspector, not a second execution/event store.
func (a *app) eventPage(ctx context.Context, r *http.Request) (any, error) {
	after := int64(0)
	if raw := r.URL.Query().Get("after"); raw != "" {
		var err error
		after, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || after < 0 {
			return nil, problem(sam.CodeInvalidArgument, "Invalid event cursor.")
		}
	}
	out, err := a.rt.Events(ctx, sam.EventQuery{SessionID: r.URL.Query().Get("session_id"), After: after, Limit: 100})
	return map[string]any{"events": out}, err
}
