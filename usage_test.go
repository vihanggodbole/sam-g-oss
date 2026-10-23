package sam

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func TestResponsesCacheUsageDecode(t *testing.T) {
	for _, cached := range []*int64{new(int64(8)), new(int64(0)), nil} {
		u := map[string]any{"input_tokens": 10, "output_tokens": 2}
		if cached != nil {
			u["input_tokens_details"] = map[string]any{"cached_tokens": *cached}
		}
		ev := map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed", "usage": u}}
		res, err := decodeStream(strings.NewReader("data: "+mustJSON(ev)+"\n\n"), DefaultMaxProviderEventBytes, func(string, string) {})
		if err != nil || mustJSON(res.usage.CacheReadTokens) != mustJSON(cached) || res.usage.CacheWriteTokens != nil || *res.usage.InputTokens != 10 {
			t.Fatal(res, err)
		}
	}
	for _, cached := range []any{int64(-1), int64(11), "eight", json.Number("9223372036854775808")} {
		ev := map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed", "usage": map[string]any{
			"input_tokens": 10, "input_tokens_details": map[string]any{"cached_tokens": cached},
		}}}
		if _, err := decodeStream(strings.NewReader("data: "+mustJSON(ev)+"\n\n"), DefaultMaxProviderEventBytes, func(string, string) {}); err == nil {
			t.Fatal("invalid cached usage accepted", cached)
		}
	}
	if _, err := addUsage(Usage{CacheReadTokens: new(int64(math.MaxInt64))}, Usage{CacheReadTokens: new(int64(1))}); err == nil {
		t.Fatal("aggregate overflow accepted")
	}
}

func TestMessagesCacheUsageComponents(t *testing.T) {
	for _, tc := range []struct {
		input, read, write, total *int64
		invalid                   bool
	}{
		{new(int64(10)), new(int64(3)), new(int64(2)), new(int64(15)), false},
		{new(int64(0)), new(int64(0)), new(int64(0)), new(int64(0)), false},
		{nil, new(int64(3)), new(int64(2)), nil, false},
		{new(int64(10)), nil, new(int64(0)), nil, false},
		{new(int64(math.MaxInt64)), new(int64(1)), new(int64(0)), nil, true},
		{new(int64(10)), new(int64(-1)), new(int64(0)), nil, true},
	} {
		events := messagesEvents("end_turn", map[string]any{"type": "text", "text": "result"})
		events[0]["message"].(map[string]any)["usage"] = map[string]any{
			"input_tokens": tc.input, "cache_read_input_tokens": tc.read, "cache_creation_input_tokens": tc.write,
		}
		var wire strings.Builder
		for _, ev := range events {
			wire.WriteString("data: " + mustJSON(ev) + "\n\n")
		}
		res, err := decodeMessages(strings.NewReader(wire.String()), DefaultMaxProviderEventBytes, func(string, string) {})
		if tc.invalid {
			if err == nil {
				t.Fatal("invalid Messages cache usage accepted")
			}
			continue
		}
		if err != nil || mustJSON(res.usage.InputTokens) != mustJSON(tc.total) || mustJSON(res.usage.CacheReadTokens) != mustJSON(tc.read) || mustJSON(res.usage.CacheWriteTokens) != mustJSON(tc.write) {
			t.Fatal(res, err)
		}
	}
}
