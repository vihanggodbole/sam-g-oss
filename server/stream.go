package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	sam "github.com/vihanggodbole/sam-g-oss"
)

// events streams a session's events as SSE: it replays everything after the
// cursor and then follows new events through the library's polling
// subscription. Ending the stream only ends this subscription.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	after, err := eventCursor(r)
	if err != nil {
		writeError(w, err)
		return
	}
	sub, err := s.rt.Subscribe(r.Context(), sam.EventQuery{SessionID: r.PathValue("id"), After: after})
	if err != nil {
		writeError(w, err)
		return
	}
	defer sub.Close()

	// net/http clears the write deadline itself once the handler returns and
	// the response is finished, so a timed-out stream cannot block there.
	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if s.send(w, rc, nil) != nil {
		return
	}
	for {
		frame, done := s.nextFrame(r.Context(), sub)
		if frame == nil || s.send(w, rc, frame) != nil || done {
			return
		}
	}
}

// nextFrame waits up to one heartbeat interval for the next event. It
// returns an event frame, a heartbeat comment, or a final error frame. A nil
// frame means the client went away.
func (s *Server) nextFrame(ctx context.Context, sub *sam.Subscription) (frame []byte, done bool) {
	wait, cancel := context.WithTimeout(ctx, s.Heartbeat)
	ev, err := sub.Next(wait)
	cancel()
	switch {
	case err == nil:
		data, err := json.Marshal(ev)
		if err != nil {
			return errorFrame(err), true
		}
		return fmt.Appendf(nil, "id: %d\nevent: %s\ndata: %s\n\n", ev.Seq, ev.Kind, data), false
	case ctx.Err() != nil:
		return nil, true
	case errors.Is(err, context.DeadlineExceeded):
		return []byte(": heartbeat\n\n"), false
	default: // session deleted, runtime closed, or storage failure
		return errorFrame(err), true
	}
}

// errorFrame reports why the stream ended. It carries no id, so the
// client's cursor is unchanged.
func errorFrame(err error) []byte {
	var e *sam.Error
	if !errors.As(err, &e) {
		e = &sam.Error{Code: "internal", Message: err.Error()}
	}
	data, _ := json.Marshal(map[string]*sam.Error{"error": e})
	return fmt.Appendf(nil, "event: error\ndata: %s\n\n", data)
}

// send writes and flushes one frame under a per-write deadline so a stalled
// client is disconnected instead of holding the stream open.
func (s *Server) send(w http.ResponseWriter, rc *http.ResponseController, frame []byte) error {
	if err := rc.SetWriteDeadline(time.Now().Add(s.WriteTimeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	if _, err := w.Write(frame); err != nil {
		return err
	}
	return rc.Flush()
}

// eventCursor reads the replay cursor from Last-Event-ID or ?after=. When
// both are present they must agree.
func eventCursor(r *http.Request) (int64, error) {
	var cursor int64 = -1
	for _, v := range []string{r.Header.Get("Last-Event-ID"), r.URL.Query().Get("after")} {
		if v == "" {
			continue
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return 0, invalid("event cursor %q must be a non-negative integer", v)
		}
		if cursor >= 0 && n != cursor {
			return 0, invalid("Last-Event-ID (%d) and after (%d) disagree", cursor, n)
		}
		cursor = n
	}
	return max(cursor, 0), nil
}
