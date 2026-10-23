package sam

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
)

// readProviderEvent is the bounded SSE capture owner for both codecs. Protocol
// validation happens after capture; overflow cancels the request without retry.
func readProviderEvent(br *bufio.Reader, maxEvent int) ([]byte, error) {
	var data []byte
	for {
		line, err := readLine(br, maxEvent-len(data))
		if errors.Is(err, io.EOF) && len(data) > 0 {
			line, err = nil, nil
		}
		if errors.Is(err, errEventTooLarge) {
			return nil, &modelError{msg: fmt.Sprintf("a provider stream event exceeded %s (max_provider_event_bytes); the request was stopped and is not retried. Raise the limit if this model legitimately sends larger events", formatBytes(maxEvent))}
		}
		if errors.Is(err, io.EOF) {
			return nil, &modelError{msg: "response stream ended without a terminal event", retryable: true}
		}
		if err != nil {
			return nil, err
		}
		if len(line) > 0 {
			if v, ok := bytes.CutPrefix(line, []byte("data:")); ok {
				if len(data) > 0 {
					data = append(data, '\n')
				}
				data = append(data, bytes.TrimPrefix(v, []byte(" "))...)
			}
			continue
		}
		if len(data) > 0 {
			return data, nil
		}
	}
}
