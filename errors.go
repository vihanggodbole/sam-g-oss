package sam

import (
	"context"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"
)

// ErrorCode is a stable, machine-readable error category.
type ErrorCode string

// Stable error codes returned by the library. Use ErrorCodeOf to classify
// wrapped errors without depending on their message text.
const (
	// CodeInvalidArgument marks invalid input or configuration.
	CodeInvalidArgument ErrorCode = "invalid_argument"
	// CodeNotFound means the requested session or file does not exist.
	CodeNotFound ErrorCode = "not_found"
	// CodeFileTooLarge means a human file read exceeded its per-open byte limit.
	CodeFileTooLarge ErrorCode = "file_too_large"
	// CodeFileReadFailed means an OS file read failed (for example permissions).
	CodeFileReadFailed ErrorCode = "file_read_failed"
	// CodeConflict means the requested operation is not valid in the current state.
	CodeConflict ErrorCode = "conflict"
	// CodeChildrenBusy rejects deletion while owned children have unfinished work.
	CodeChildrenBusy ErrorCode = "children_busy"
	// CodeUnsupported means a model option or requested file type is unsupported.
	CodeUnsupported ErrorCode = "unsupported"
	// CodeProviderError marks a failed model request after work was accepted.
	CodeProviderError ErrorCode = "provider_error"
	// CodeAuthRequired means a subscription account is missing, logged out,
	// of the wrong provider, or its credentials were rejected or could not be
	// refreshed. The user must log in again or select another account.
	CodeAuthRequired ErrorCode = "auth_required"
	// CodeHistoryIncompatible means the selected provider route rejected
	// required saved history, such as opaque reasoning produced under another
	// account. History is kept unchanged.
	CodeHistoryIncompatible ErrorCode = "history_incompatible"
	// CodeContextLimit means the requested context cannot fit its configured budget.
	CodeContextLimit ErrorCode = "context_limit"
	// CodeStorageError means persistence failed and the runtime stopped admission.
	CodeStorageError ErrorCode = "storage_error"
	// CodeStorageBusy means a new mutation was not accepted because SQLite stayed busy.
	CodeStorageBusy ErrorCode = "storage_busy"
	// CodeProcessObservation means child-process cleanup could not be confirmed.
	CodeProcessObservation ErrorCode = "process_observation_failed"
	// CodeClosed means the runtime or subscription is closing or closed.
	CodeClosed ErrorCode = "closed"
)

// Error is the typed error returned when a public operation fails synchronously.
// Once work is accepted, later failures are recorded on the turn or operation
// and reported through events instead of being returned by the accepting call.
type Error struct {
	// Code is stable for programmatic classification.
	Code ErrorCode `json:"code"`
	// Message describes the failure for people and logs; do not parse it.
	Message string `json:"message"`
	// SessionID and OperationID identify related work when available.
	SessionID       string           `json:"session_id,omitempty"`
	OperationID     string           `json:"operation_id,omitempty"`
	ProviderFailure *ProviderFailure `json:"provider_failure,omitempty"`
}

// ProviderFailure contains safe upstream facts, never a raw response body.
// ResetAt is only populated from documented facts for this exact limit.
type ProviderFailure struct {
	HTTPStatus        int        `json:"http_status,omitempty"`
	Code              string     `json:"code,omitempty"`
	Param             string     `json:"param,omitempty"`
	RequestID         string     `json:"request_id,omitempty"`
	Message           string     `json:"message"`
	Shape             string     `json:"shape"`
	Retryable         bool       `json:"retryable"`
	RetryAfterSeconds *float64   `json:"retry_after_seconds,omitempty"`
	ResetAt           *time.Time `json:"reset_at,omitempty"`
}

func (e *Error) Error() string { return string(e.Code) + ": " + e.Message }

func errorf(code ErrorCode, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// ErrorCodeOf returns the code of an *Error in err's wrapping chain, or the
// empty code when err is not a sam error.
func ErrorCodeOf(err error) ErrorCode {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

func storageErr(err error) error {
	if err == nil {
		return nil
	}
	var e *Error
	if errors.As(err, &e) {
		return err
	}
	return &Error{Code: CodeStorageError, Message: "persistence failed: " + err.Error()}
}

var (
	errNotFound   = errorf(CodeNotFound, "session not found")
	errBusy       = errorf(CodeConflict, "Session is running. Interrupt it or wait before retrying.")
	errClosed     = errorf(CodeClosed, "runtime is closed")
	errBoundModel = errorf(CodeConflict, "This session already has messages. Use SwitchModel to explicitly change its model or account for future work.")
)

// truncate shortens s to at most n bytes on a UTF-8 boundary and marks the
// cut, so a shortened diagnostic is never mistaken for the whole message.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return fmt.Sprintf("%s… [truncated %d bytes]", s[:cut], len(s)-cut)
}

// readErr reports a failed read-only query: the caller's own cancellation
// is returned as is, anything else as a storage error.
func readErr(ctx context.Context, err error) error {
	if err != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return storageErr(err)
}
