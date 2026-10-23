package sam

import (
	"bufio"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/iotest"
)

func TestDecodeFragmentedInterleavedStream(t *testing.T) {
	stream := "event: response.output_text.delta\n" +
		"data: {\"type\":\"response.output_text.delta\",\"output_index\":1,\n" +
		"data: \"delta\":\"Hel\"}\n\n" +
		": comment\n\n" +
		"data: {\"type\":\"response.reasoning_summary_text.delta\",\"output_index\":0,\"delta\":\"think\"}\r\n\r\n" +
		"data: {\"type\":\"response.function_call_arguments.delta\",\"delta\":\"{\\\"par\"}\n\n" +
		"data: {\"type\":\"response.output_item.done\",\"output_index\":2,\"item\":{\"type\":\"function_call\",\"call_id\":\"c\",\"name\":\"bash\",\"arguments\":\"{}\"}}\n\n" +
		"data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"reasoning\",\"encrypted_content\":\"opaque\",\"summary\":[]}}\n\n" +
		"data: {\"type\":\"response.output_text.delta\",\"output_index\":1,\"delta\":\"lo\"}\n\n" +
		"data: {\"type\":\"response.output_item.done\",\"output_index\":1,\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"Hello\"}]}}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[]}}"
	var text, reasoning string
	const limit = DefaultMaxProviderEventBytes
	res, err := decodeStream(iotest.OneByteReader(strings.NewReader(stream)), limit, func(k, d string) {
		if k == "text" {
			text += d
		} else {
			reasoning += d
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if text != "Hello" || reasoning != "think" || len(res.output) != 3 || !strings.Contains(string(res.output[0]), "opaque") {
		t.Fatalf("text=%q reasoning=%q output=%d", text, reasoning, len(res.output))
	}
	if _, err := decodeStream(strings.NewReader("data: {\"type\":\"response.output_text.delta\",\"delta\":\"x\"}\n\n"), limit, func(string, string) {}); err == nil {
		t.Fatal("EOF without terminal event must fail")
	}
	_, err = decodeStream(strings.NewReader("data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"server_error\",\"message\":\"boom\"}}}\n\n"), limit, nil)
	if me, ok := err.(*modelError); !ok || !me.retryable {
		t.Fatalf("failed event: %v", err)
	}
	if _, err := decodeStream(io.MultiReader(strings.NewReader("data: "), strings.NewReader(strings.Repeat("x", 101))), 100, nil); err == nil {
		t.Fatal("oversized line accepted")
	}
	// Carriage returns are buffered bytes too: a long run of them cannot
	// slip past the bound by being trimmed before the length check.
	crs := "data: " + strings.Repeat("\r", 256<<10) + "\n"
	if _, err := readLine(bufio.NewReaderSize(strings.NewReader(crs), 64<<10), 100); !errors.Is(err, errEventTooLarge) {
		t.Fatalf("carriage-return run: %v", err)
	}
	// Only the line terminator is removed; other trailing CRs are data.
	for in, want := range map[string]string{"data: x\r\n": "data: x", "data: x\n": "data: x", "data: x\r\r\n": "data: x\r"} {
		if got, err := readLine(bufio.NewReader(strings.NewReader(in)), 100); err != nil || string(got) != want {
			t.Fatalf("%q: %q %v", in, got, err)
		}
	}
	// The bound covers the assembled event, not just each line: three
	// 40-byte data lines exceed 100 bytes together.
	line := "data: " + strings.Repeat("x", 40) + "\n"
	_, err = decodeStream(strings.NewReader(line+line+line+"\n"), 100, nil)
	if me, ok := err.(*modelError); !ok || me.retryable || !strings.Contains(me.msg, "max_provider_event_bytes") {
		t.Fatalf("oversized multi-line event: %v", err)
	}
}

func TestDiagnosticsRedactBeforeShortening(t *testing.T) {
	const key = "sk-synthetic-0123456789abcdef"
	// The key sits across the 1024-byte display bound.
	body := `{"error":{"message":"` + strings.Repeat("x", 1000) + " " + key + `"}}`
	resp := &http.Response{StatusCode: 400, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
	msg := statusError(resp, key).Error()
	if strings.Contains(msg, key[:8]) || !strings.Contains(msg, "[truncated") {
		t.Fatalf("diagnostic leaks or hides shortening: %q", msg[len(msg)-80:])
	}
	// A key cut off by a read limit at the very end is redacted too.
	if got := redact("body ends with "+key[:12], key); strings.Contains(got, key[:6]) {
		t.Fatal(got)
	}
}
