package server

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHostRoutesShareBrowserGuard(t *testing.T) {
	s := New(nil) // No runtime is needed for this host-owned route.
	if err := s.SetAllowedOrigins([]string{"https://web.example"}); err != nil {
		t.Fatal(err)
	}
	s.Handle("POST /v1/host-example", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	for _, tc := range []struct {
		method, host, origin string
		status               int
	}{
		{"POST", "100.64.1.2:8080", "https://web.example", http.StatusCreated},
		{"POST", "100.64.1.2:8080", "https://evil.example", http.StatusForbidden},
		{"POST", "other.example:8080", "https://web.example", http.StatusForbidden},
		{"OPTIONS", "100.64.1.2:8080", "https://web.example", http.StatusNoContent},
	} {
		r := httptest.NewRequest(tc.method, "http://"+tc.host+"/v1/host-example", nil)
		r.Header.Set("Origin", tc.origin)
		if tc.method == "OPTIONS" {
			r.Header.Set("Access-Control-Request-Method", "POST")
			r.Header.Set("Access-Control-Request-Headers", "content-type")
		}
		local := &net.TCPAddr{IP: net.ParseIP("100.64.1.2"), Port: 8080}
		r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, local))
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Errorf("%s %s from %s: got %d, want %d", tc.method, tc.host, tc.origin, w.Code, tc.status)
		}
		if tc.status != http.StatusForbidden && w.Header().Get("Access-Control-Allow-Origin") != tc.origin {
			t.Error("allowed host route lost its CORS header")
		}
	}
}
