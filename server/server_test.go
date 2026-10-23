package server

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDecodeRequiresExactlyOneValue(t *testing.T) {
	s := &Server{MaxBodyBytes: 64}
	for raw, ok := range map[string]bool{
		`{"prompt":"hi"}`:                           true,
		"{\"prompt\":\"hi\"}\n  ":                   true,
		`{"prompt":"hi"}]`:                          false,
		`{"prompt":"hi"}}`:                          false,
		`{"prompt":"hi"} {}`:                        false,
		`{"prompt":"hi"} x`:                         false,
		`{"prompt":"hi"}` + strings.Repeat(" ", 64): false, // over the configured limit
	} {
		var out struct {
			Prompt string `json:"prompt"`
		}
		err := s.decode(httptest.NewRequest("POST", "/", strings.NewReader(raw)), &out, false)
		if (err == nil) != ok {
			t.Errorf("%q: %v", raw, err)
		}
	}
}
