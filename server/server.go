// Package server exposes a sam.Runtime over HTTP and Server-Sent Events.
// It only translates requests into library calls; all agent behavior,
// persistence, and deduplication live in the library.
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	sam "github.com/vihanggodbole/sam-g-oss"
)

// Defaults for the Server's adjustable limits.
const (
	// DefaultMaxBodyBytes bounds JSON request bodies. It is generous so that
	// large prompts and configurations fit; it only stops runaway input.
	DefaultMaxBodyBytes = 8 << 20
	// DefaultWriteTimeout bounds each SSE write.
	DefaultWriteTimeout = 30 * time.Second
	// DefaultHeartbeat is the idle interval between SSE keep-alive comments.
	DefaultHeartbeat = 15 * time.Second
)

// codeForbiddenOrigin rejects untrusted request origins or authorities.
const codeForbiddenOrigin sam.ErrorCode = "forbidden_origin"

// Server is an http.Handler for one runtime. The runtime's lifetime is owned
// by the caller; closing requests or streams never affects running work.
type Server struct {
	rt             *sam.Runtime
	mux            *http.ServeMux
	allowedOrigins map[string]bool
	// Catalogs optionally enables account-scoped installation; set before serving.
	Catalogs CatalogHost

	// Heartbeat is the idle interval between SSE keep-alive comments.
	Heartbeat time.Duration
	// WriteTimeout bounds each SSE write; a stalled client is disconnected
	// and can reconnect with its cursor.
	WriteTimeout time.Duration
	// MaxBodyBytes bounds JSON request bodies.
	MaxBodyBytes int64
}

// New returns a handler serving rt with the default limits; set the
// exported fields before serving to change them.
func New(rt *sam.Runtime) *Server {
	s := &Server{
		rt: rt, mux: http.NewServeMux(),
		Heartbeat: DefaultHeartbeat, WriteTimeout: DefaultWriteTimeout, MaxBodyBytes: DefaultMaxBodyBytes,
	}
	s.routes()
	return s
}

// Handle registers a host-owned route behind the same Host/Origin checks as
// the built-in API. Call before serving. Patterns and conflicts follow
// http.ServeMux.Handle; the handler owns its body format, limits and responses.
func (s *Server) Handle(pattern string, handler http.Handler) {
	s.mux.Handle(pattern, handler)
}

// ServeHTTP accepts same-origin browsers and explicitly trusted frontends.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !s.browserAccess(w, r) {
		writeError(w, &sam.Error{
			Code:    codeForbiddenOrigin,
			Message: "untrusted request origin or Host; use the listener's IP/localhost, or explicitly configure the frontend/proxy origin",
		})
		return
	}
	if r.Method == http.MethodOptions && r.Header.Get("Origin") != "" && r.Header.Get("Access-Control-Request-Method") != "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.mux.ServeHTTP(w, r)
}

// apiFunc handles one JSON route and returns the status and response body.
type apiFunc func(r *http.Request) (int, any, error)

func api(fn apiFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		status, body, err := fn(r)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, status, body)
	}
}

func (s *Server) routes() {
	m := s.mux
	m.HandleFunc("GET /healthz", api(func(*http.Request) (int, any, error) {
		return http.StatusOK, map[string]string{"status": "ok"}, nil
	}))
	m.HandleFunc("GET /readyz", api(func(*http.Request) (int, any, error) {
		if err := s.rt.Ready(); err != nil {
			return 0, nil, err
		}
		return http.StatusOK, map[string]string{"status": "ready"}, nil
	}))
	m.HandleFunc("GET /v1/providers", api(func(*http.Request) (int, any, error) {
		return http.StatusOK, map[string]any{"providers": s.rt.Providers()}, nil
	}))
	m.HandleFunc("GET /v1/providers/{provider}/models", api(s.discoverCatalog))
	m.HandleFunc("PUT /v1/providers/{provider}/models", api(s.installCatalog))
	m.HandleFunc("GET /v1/models", api(func(*http.Request) (int, any, error) {
		return http.StatusOK, map[string]any{"models": s.rt.Models(), "catalog_installation": s.Catalogs != nil, "model_switching": true}, nil
	}))
	m.HandleFunc("GET /v1/tools", api(func(*http.Request) (int, any, error) {
		return http.StatusOK, map[string]any{"tools": s.rt.Tools()}, nil
	}))
	m.HandleFunc("GET /v1/diagnostics", api(func(*http.Request) (int, any, error) {
		return http.StatusOK, map[string]any{"diagnostics": nonNil(s.rt.Diagnostics())}, nil
	}))

	m.HandleFunc("POST /v1/sessions", api(s.createSession))
	m.HandleFunc("GET /v1/sessions", api(s.listSessions))
	m.HandleFunc("GET /v1/sessions/recent", api(s.recentSessions))
	m.HandleFunc("GET /v1/workspaces", api(s.listWorkspaces))
	m.HandleFunc("GET /v1/sessions/{id}", api(func(r *http.Request) (int, any, error) {
		out, err := s.rt.GetSession(r.Context(), r.PathValue("id"))
		return http.StatusOK, out, err
	}))
	m.HandleFunc("PATCH /v1/sessions/{id}", api(s.renameSession))
	m.HandleFunc("DELETE /v1/sessions/{id}", api(func(r *http.Request) (int, any, error) {
		err := s.rt.DeleteSession(r.Context(), sam.DeleteSessionRequest{RequestID: requestID(r), SessionID: r.PathValue("id")})
		return http.StatusNoContent, nil, err
	}))

	m.HandleFunc("POST /v1/sessions/{id}/subagents", api(s.startSubagent))
	m.HandleFunc("GET /v1/sessions/{id}/subagents", api(func(r *http.Request) (int, any, error) {
		out, err := s.rt.Subagents(r.Context(), r.PathValue("id"))
		return http.StatusOK, map[string]any{"subagents": nonNil(out)}, err
	}))
	m.HandleFunc("POST /v1/sessions/{id}/turns", api(s.startTurn))
	m.HandleFunc("GET /v1/sessions/{id}/turns", api(s.listTurns))
	m.HandleFunc("POST /v1/sessions/{id}/continue", api(s.continueSession))
	m.HandleFunc("POST /v1/sessions/{id}/steering", api(s.steer))
	m.HandleFunc("GET /v1/sessions/{id}/steering", api(func(r *http.Request) (int, any, error) {
		out, err := s.rt.ListSteering(r.Context(), r.PathValue("id"))
		return http.StatusOK, map[string]any{"steering": nonNil(out)}, err
	}))
	m.HandleFunc("DELETE /v1/sessions/{id}/steering", api(func(r *http.Request) (int, any, error) {
		out, err := s.rt.ClearSteering(r.Context(), sessionRequest(r))
		out.Cleared = nonNil(out.Cleared)
		return http.StatusOK, out, err
	}))
	m.HandleFunc("POST /v1/sessions/{id}/interrupt", api(func(r *http.Request) (int, any, error) {
		req := sessionRequest(r)
		err := s.rt.Interrupt(r.Context(), req)
		return http.StatusAccepted, map[string]string{"session_id": req.SessionID}, err
	}))
	m.HandleFunc("POST /v1/sessions/{id}/compact", api(func(r *http.Request) (int, any, error) {
		out, err := s.rt.Compact(r.Context(), sessionRequest(r))
		return http.StatusAccepted, out, err
	}))

	m.HandleFunc("GET /v1/sessions/{id}/config", api(func(r *http.Request) (int, any, error) {
		out, err := s.rt.GetConfig(r.Context(), r.PathValue("id"))
		return http.StatusOK, out, err
	}))
	m.HandleFunc("PUT /v1/sessions/{id}/config", api(s.updateConfig))
	m.HandleFunc("POST /v1/sessions/{id}/model", api(s.switchModel))
	m.HandleFunc("POST /v1/sessions/{id}/reload", api(func(r *http.Request) (int, any, error) {
		var body struct{}
		if err := s.decode(r, &body, true); err != nil {
			return 0, nil, err
		}
		out, err := s.rt.ReloadSession(r.Context(), sessionRequest(r))
		return http.StatusOK, out, err
	}))
	m.HandleFunc("GET /v1/sessions/{id}/context", api(func(r *http.Request) (int, any, error) {
		out, err := s.rt.InspectContext(r.Context(), r.PathValue("id"))
		return http.StatusOK, out, err
	}))
	m.HandleFunc("GET /v1/sessions/{id}/file", func(w http.ResponseWriter, r *http.Request) {
		// User-requested files may contain secrets, unlike safe status metadata.
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		api(func(r *http.Request) (int, any, error) {
			limit := 0
			if raw := r.URL.Query().Get("limit_bytes"); raw != "" {
				var err error
				limit, err = strconv.Atoi(raw)
				if err != nil {
					return 0, nil, invalid("limit_bytes must be an integer byte count representable on this backend (zero selects its default)")
				}
			}
			out, err := s.rt.ReadFile(r.Context(), sam.ReadFileRequest{
				SessionID: r.PathValue("id"), Path: r.URL.Query().Get("path"), LimitBytes: limit,
			})
			return http.StatusOK, out, err
		})(w, r)
	})
	m.HandleFunc("GET /v1/sessions/{id}/history", api(s.history))
	m.HandleFunc("GET /v1/sessions/{id}/events", s.events)
	s.authRoutes()

	m.HandleFunc("/", api(func(r *http.Request) (int, any, error) {
		return 0, nil, &sam.Error{Code: sam.CodeNotFound, Message: "no route for " + r.Method + " " + r.URL.Path}
	}))
}

func (s *Server) createSession(r *http.Request) (int, any, error) {
	var body struct {
		Name      string                `json:"name"`
		Workspace string                `json:"workspace"`
		Config    *sam.SessionOverrides `json:"config"`
	}
	if err := s.decode(r, &body, false); err != nil {
		return 0, nil, err
	}
	out, err := s.rt.CreateSession(r.Context(), sam.CreateSessionRequest{
		RequestID: requestID(r), Name: body.Name, Workspace: body.Workspace, Config: body.Config,
	})
	return http.StatusCreated, out, err
}

func (s *Server) listSessions(r *http.Request) (int, any, error) {
	limit, err := intParam(r, "limit")
	if err != nil {
		return 0, nil, err
	}
	out, err := s.rt.ListSessions(r.Context(), sam.ListQuery{After: r.URL.Query().Get("after"), Limit: int(limit)})
	return http.StatusOK, out, err
}

func (s *Server) recentSessions(r *http.Request) (int, any, error) {
	limit, err := intParam(r, "limit")
	if err != nil {
		return 0, nil, err
	}
	v := r.URL.Query()
	active := v.Get("active")
	if active != "" && active != "true" && active != "false" {
		return 0, nil, invalid("active must be true or false")
	}
	out, err := s.rt.RecentSessions(r.Context(), sam.RecentQuery{
		Workspace: v.Get("workspace"), Parent: v.Get("parent"), Search: v.Get("q"), Active: active == "true",
		After: v.Get("after"), Limit: int(limit),
	})
	return http.StatusOK, out, err
}

func (s *Server) listWorkspaces(r *http.Request) (int, any, error) {
	limit, err := intParam(r, "limit")
	if err != nil {
		return 0, nil, err
	}
	out, err := s.rt.Workspaces(r.Context(), sam.ListQuery{After: r.URL.Query().Get("after"), Limit: int(limit)})
	return http.StatusOK, out, err
}

func (s *Server) renameSession(r *http.Request) (int, any, error) {
	var body struct {
		Name string `json:"name"`
	}
	if err := s.decode(r, &body, false); err != nil {
		return 0, nil, err
	}
	out, err := s.rt.RenameSession(r.Context(), sam.RenameSessionRequest{
		RequestID: requestID(r), SessionID: r.PathValue("id"), Name: body.Name,
	})
	return http.StatusOK, out, err
}

func (s *Server) startSubagent(r *http.Request) (int, any, error) {
	var body struct {
		Name      string                `json:"name"`
		Prompt    string                `json:"prompt"`
		Workspace string                `json:"workspace"`
		Config    *sam.SessionOverrides `json:"config"`
	}
	if err := s.decode(r, &body, false); err != nil {
		return 0, nil, err
	}
	out, err := s.rt.StartSubagent(r.Context(), sam.StartSubagentRequest{
		RequestID: requestID(r), ParentID: r.PathValue("id"), Name: body.Name,
		Prompt: body.Prompt, Workspace: body.Workspace, Config: body.Config,
	})
	return http.StatusAccepted, out, err
}

func (s *Server) startTurn(r *http.Request) (int, any, error) {
	var body struct {
		Prompt string      `json:"prompt"`
		Images []sam.Image `json:"images"`
	}
	if err := s.decode(r, &body, false); err != nil {
		return 0, nil, err
	}
	out, err := s.rt.StartTurn(r.Context(), sam.StartTurnRequest{
		RequestID: requestID(r), SessionID: r.PathValue("id"), Prompt: body.Prompt, Images: body.Images,
	})
	return http.StatusAccepted, out, err
}

func (s *Server) listTurns(r *http.Request) (int, any, error) {
	limit, err := intParam(r, "limit")
	if err != nil {
		return 0, nil, err
	}
	v := r.URL.Query()
	before := v.Has("before")
	if before && v.Has("after") {
		return 0, nil, invalid("after and before cannot be used together")
	}
	beforeCursor := ""
	if before {
		n, err := intParam(r, "before")
		if err != nil {
			return 0, nil, err
		}
		beforeCursor = strconv.FormatInt(n, 10)
	}
	summary := false
	if v.Has("summary") {
		if v.Get("summary") != "true" && v.Get("summary") != "false" {
			return 0, nil, invalid("summary must be true or false")
		}
		summary = v.Get("summary") == "true"
	}
	turns, cursor, err := s.rt.ListTurns(r.Context(), sam.ListQuery{
		SessionID: r.PathValue("id"), After: v.Get("after"), Before: beforeCursor,
		Summary: summary, Limit: int(limit),
	})
	page := map[string]any{"turns": nonNil(turns)}
	if before {
		if cursor != "" {
			page["older"] = cursor
		}
	} else {
		page["next"] = cursor
	}
	return http.StatusOK, page, err
}

func (s *Server) continueSession(r *http.Request) (int, any, error) {
	var body struct {
		Prompt string `json:"prompt"`
	}
	if err := s.decode(r, &body, true); err != nil {
		return 0, nil, err
	}
	out, err := s.rt.Continue(r.Context(), sam.ContinueRequest{
		RequestID: requestID(r), SessionID: r.PathValue("id"), Prompt: body.Prompt,
	})
	return http.StatusAccepted, out, err
}

func (s *Server) steer(r *http.Request) (int, any, error) {
	var body struct {
		Text string `json:"text"`
	}
	if err := s.decode(r, &body, false); err != nil {
		return 0, nil, err
	}
	out, err := s.rt.Steer(r.Context(), sam.SteerRequest{
		RequestID: requestID(r), SessionID: r.PathValue("id"), Text: body.Text,
	})
	return http.StatusAccepted, out, err
}

func (s *Server) updateConfig(r *http.Request) (int, any, error) {
	var cfg sam.SessionOverrides
	if err := s.decode(r, &cfg, false); err != nil {
		return 0, nil, err
	}
	out, err := s.rt.UpdateConfig(r.Context(), sam.UpdateConfigRequest{
		RequestID: requestID(r), SessionID: r.PathValue("id"), Config: cfg,
	})
	return http.StatusOK, out, err
}

func (s *Server) history(r *http.Request) (int, any, error) {
	v := r.URL.Query()
	after, err := intParam(r, "after")
	if err != nil {
		return 0, nil, err
	}
	limit, err := intParam(r, "limit")
	if err != nil {
		return 0, nil, err
	}
	if v.Has("after") && v.Has("before") {
		return 0, nil, invalid("after and before cannot be used together")
	}
	var before *int64
	if v.Has("before") {
		n, err := intParam(r, "before")
		if err != nil {
			return 0, nil, err
		}
		before = &n
	}
	out, err := s.rt.History(r.Context(), sam.HistoryQuery{
		SessionID: r.PathValue("id"), After: after, Before: before, Limit: int(limit),
	})
	out.Items = nonNil(out.Items)
	return http.StatusOK, out, err
}

// requestID forwards the client's Idempotency-Key. The library requires it
// on every mutation and rejects a missing key as invalid_argument.
func requestID(r *http.Request) string { return r.Header.Get("Idempotency-Key") }

func sessionRequest(r *http.Request) sam.SessionRequest {
	return sam.SessionRequest{RequestID: requestID(r), SessionID: r.PathValue("id")}
}

// decode reads exactly one JSON value into v, rejecting unknown fields and
// anything after it except whitespace. An empty body is allowed only when
// optional is set.
func (s *Server) decode(r *http.Request, v any, optional bool) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, s.MaxBodyBytes))
	dec.DisallowUnknownFields()
	err := dec.Decode(v)
	if optional && errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		var extra json.RawMessage
		if err = dec.Decode(&extra); errors.Is(err, io.EOF) {
			return nil
		} else if err == nil {
			err = errors.New("unexpected data after the JSON value")
		}
	}
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		return invalid("request body exceeds %d bytes (server.max_body_bytes)", s.MaxBodyBytes)
	case errors.Is(err, io.EOF):
		return invalid("a JSON request body is required")
	}
	return invalid("invalid JSON body: %v", err)
}

func intParam(r *http.Request, name string) (int64, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0, invalid("%s must be a non-negative integer", name)
	}
	return n, nil
}

func invalid(format string, args ...any) error {
	return &sam.Error{Code: sam.CodeInvalidArgument, Message: fmt.Sprintf(format, args...)}
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// statusOf maps stable library error codes to HTTP statuses.
func statusOf(code sam.ErrorCode) int {
	switch code {
	case sam.CodeInvalidArgument:
		return http.StatusBadRequest
	case codeForbiddenOrigin:
		return http.StatusForbidden
	case sam.CodeNotFound:
		return http.StatusNotFound
	case sam.CodeConflict, sam.CodeChildrenBusy, sam.CodeUnsupported, sam.CodeAuthRequired, sam.CodeHistoryIncompatible:
		return http.StatusConflict
	case sam.CodeFileTooLarge:
		return http.StatusRequestEntityTooLarge
	case sam.CodeContextLimit, sam.CodeFileReadFailed:
		return http.StatusUnprocessableEntity
	case sam.CodeProviderError:
		return http.StatusBadGateway
	case sam.CodeProcessObservation:
		return http.StatusInternalServerError
	case sam.CodeStorageError, sam.CodeStorageBusy, sam.CodeClosed:
		return http.StatusServiceUnavailable
	}
	return http.StatusInternalServerError
}

func writeError(w http.ResponseWriter, err error) {
	var e *sam.Error
	if !errors.As(err, &e) {
		e = &sam.Error{Code: "internal", Message: err.Error()}
	}
	writeJSON(w, statusOf(e.Code), map[string]*sam.Error{"error": e})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	if body == nil {
		w.WriteHeader(status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
