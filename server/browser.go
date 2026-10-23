package server

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// SetAllowedOrigins trusts additional exact HTTP(S) origins for browser access
// and Host validation. Configure before serving, never concurrently with requests.
// Same-origin access via the listener's IP (or localhost for loopback) already
// works without configuration. Add a separate frontend or a proxy's public origin
// explicitly. Wildcards, credentials, paths and opaque/null origins are rejected.
// This grants access to the entire API; it is not user authentication.
func (s *Server) SetAllowedOrigins(origins []string) error {
	allowed := make(map[string]bool, len(origins))
	for i, raw := range origins {
		origin := canonicalOrigin(raw)
		if origin == "" {
			return fmt.Errorf("allowed origin %d must be an exact http(s)://host[:port], without credentials, a path or wildcard", i)
		}
		allowed[origin] = true
	}
	s.allowedOrigins = allowed
	return nil
}

func canonicalOrigin(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Host == "" ||
		u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.ContainsAny(raw, "*#\\ \t\r\n") {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if port == "" {
		port = "80"
		if u.Scheme == "https" {
			port = "443"
		}
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 || host == "" {
		return ""
	}
	return u.Scheme + "://" + net.JoinHostPort(host, strconv.Itoa(n))
}

func scheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

func (s *Server) trustsOrigin(r *http.Request, origin string) bool {
	if origin == "" {
		return false
	}
	if s.allowedOrigins[origin] {
		return true
	}
	addr, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok {
		return false
	}
	host, port, err := net.SplitHostPort(addr.String())
	ip := net.ParseIP(host)
	if err != nil || ip == nil || ip.IsUnspecified() {
		return false
	}
	if origin == canonicalOrigin(scheme(r)+"://"+net.JoinHostPort(host, port)) {
		return true
	}
	return ip.IsLoopback() && origin == canonicalOrigin(scheme(r)+"://"+net.JoinHostPort("localhost", port))
}

// Browser requests are supported, not bypassed. Origin checking prevents a
// foreign page from controlling this unauthenticated coding API; checking the
// target against the actual listener also prevents same-origin DNS rebinding.
// Forwarded/X-Forwarded-* headers never establish trust.
func (s *Server) browserAccess(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Add("Vary", "Origin")
	target := canonicalOrigin(scheme(r) + "://" + r.Host)
	_, networkRequest := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	raw := r.Header.Get("Origin")
	site := r.Header.Get("Sec-Fetch-Site")
	browser := raw != "" || (site != "" && site != "none")
	if networkRequest || len(s.allowedOrigins) > 0 || browser {
		if !s.trustsOrigin(r, target) {
			return false
		}
	}
	if len(r.Header.Values("Origin")) > 1 {
		return false
	}
	if raw == "" {
		return site == "" || site == "none" || site == "same-origin"
	}
	if !s.trustsOrigin(r, canonicalOrigin(raw)) {
		return false
	}
	if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
		if !preflightAllowed(r) {
			return false
		}
		w.Header().Add("Vary", "Access-Control-Request-Method")
		w.Header().Add("Vary", "Access-Control-Request-Headers")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Idempotency-Key, Last-Event-ID, Accept")
	}
	// Echo only an origin that was explicitly validated, never '*'. Cookies
	// are not API authentication, so credentialed CORS is not enabled.
	w.Header().Set("Access-Control-Allow-Origin", raw)
	return true
}

func preflightAllowed(r *http.Request) bool {
	switch r.Header.Get("Access-Control-Request-Method") {
	case "GET", "POST", "PUT", "PATCH", "DELETE":
	default:
		return false
	}
	for _, header := range strings.Split(r.Header.Get("Access-Control-Request-Headers"), ",") {
		switch strings.ToLower(strings.TrimSpace(header)) {
		case "", "content-type", "idempotency-key", "last-event-id", "accept":
		default:
			return false
		}
	}
	return true
}
