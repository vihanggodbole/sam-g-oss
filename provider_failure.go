package sam

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type wireFailure struct {
	Code    string `json:"code"`
	Type    string `json:"type"`
	Param   string `json:"param"`
	Message string `json:"message"`
}

func statusError(resp *http.Response, secrets ...string) *modelError {
	return statusErrorForAPI(resp, APIResponses, secrets...)
}

func statusErrorForAPI(resp *http.Response, api ModelAPI, secrets ...string) *modelError {
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 4097))
	if len(raw) > 4096 || readErr != nil {
		// Do not turn a truncated, unclassified plan error into a generic
		// 429/503 retry. Preserve the known status/request ID, not a raw bag.
		msg := "provider error body could not be read within the 4 KiB diagnostic bound; its code is unknown and this request is not retried"
		return sanitizeModelError(&modelError{msg: msg, failure: &ProviderFailure{HTTPStatus: resp.StatusCode, RequestID: resp.Header.Get("x-request-id"), Message: msg, Shape: "unknown"}}, secrets...)
	}
	var envelope struct {
		Error  json.RawMessage `json:"error"`
		Detail json.RawMessage `json:"detail"`
	}
	f := &ProviderFailure{HTTPStatus: resp.StatusCode, Shape: "unknown", RequestID: resp.Header.Get("x-request-id")}
	var wire wireFailure
	if json.Unmarshal(raw, &envelope) == nil {
		switch {
		case len(envelope.Error) > 0 && json.Unmarshal(envelope.Error, &wire) == nil:
			f.Shape = "error"
		case len(envelope.Detail) > 0:
			if json.Unmarshal(envelope.Detail, &wire.Message) == nil || json.Unmarshal(envelope.Detail, &wire) == nil {
				f.Shape = "detail"
			}
		}
	}
	if api == APIAnthropicMessages && wire.Code == "" {
		wire.Code = wire.Type
	}
	// HTTP failures classify only their already-safe, bounded structured
	// fields. The public outer message is bounded after it is assembled.
	f.Code = diagnostic(wire.Code, secrets...)
	f.Param = diagnostic(wire.Param, secrets...)
	f.Message = diagnostic(wire.Message, secrets...)
	if f.Message == "" {
		f.Message = "provider request failed"
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		f.Message = "unexpected redirect refused to protect credentials"
	}
	delay, known := parseRetryAdvice(resp.Header.Get("Retry-After"), time.Now())
	if known {
		f.RetryAfterSeconds = new(delay.Seconds())
	}
	e := providerModelError(f)
	if api == APIAnthropicMessages {
		e.incompatible = false
	} // no native history classification from prose
	if strings.Contains(wire.Type, "insufficient_quota") || strings.Contains(wire.Type, "billing") {
		e.retryable = false
		f.Retryable = false
	}
	e.retryAfter = delay
	return sanitizeModelError(e, secrets...)
}

// sanitizeModelError is the endpoint-aware public boundary: remove complete
// credentials before bounding either the message or its structured fields.
func sanitizeModelError(e *modelError, secrets ...string) *modelError {
	secrets = append(secrets, e.secrets...)
	e.secrets = nil // request-scoped only, never part of durable/public errors
	e.msg = diagnostic(e.msg, secrets...)
	if f := e.failure; f != nil {
		f.Code = diagnostic(f.Code, secrets...)
		f.Param = diagnostic(f.Param, secrets...)
		f.Message = diagnostic(f.Message, secrets...)
		f.RequestID = diagnostic(f.RequestID, secrets...)
	}
	return e
}

func providerModelError(f *ProviderFailure) *modelError {
	retry := false
	switch f.Code {
	case "subscription_sharing_usage_unavailable", "subscription_sharing_user_unavailable", "user_unavailable":
		retry = true
	case "subscription_sharing_usage_limit_exceeded", "subscription_sharing_user_not_eligible", "subscription_sharing_unsupported_capability", "subscription_sharing_route_not_supported", "subscription_sharing_invalid_user":
	case "insufficient_quota", "context_length_exceeded", "invalid_api_key", "invalid_encrypted_content", "usage_limit_reached", "usage_not_included":
	case "server_error", "rate_limit_exceeded", "overloaded_error", "api_error", "rate_limit_error":
		retry = true
	default:
		switch f.HTTPStatus {
		case 408, 429, 500, 502, 503, 504:
			retry = true
		}
		if strings.HasPrefix(f.Code, "subscription_sharing_") || strings.HasPrefix(f.Code, "chatpass_") || strings.Contains(f.Code, "billing") {
			retry = false
		}
	}
	f.Retryable = retry
	return &modelError{msg: providerFailureMessage(f), retryable: retry, failure: f, incompatible: rejectsHistory(f.Code, f.Message), authRequired: f.Code == "invalid_api_key" || f.Code == "invalid_token" || f.Code == "authentication_error" || f.Code == "subscription_sharing_invalid_user"}
}

func providerFailureMessage(f *ProviderFailure) string {
	status := f.Code
	if f.HTTPStatus != 0 {
		status = fmt.Sprintf("HTTP %d, %s", f.HTTPStatus, f.Code)
	}
	return fmt.Sprintf("provider failure (%s): %s", status, f.Message)
}

func providerFailureOf(err error) *ProviderFailure {
	var e *Error
	if errors.As(err, &e) {
		return e.ProviderFailure
	}
	return nil
}

// authorizeOutput records only offered, correctly namespaced local targets.
// Empty target is an explicit denial; restart must never reinterpret it.
func authorizeOutput(res *modelResult, req modelRequest) {
	res.dispatch = map[string]string{}
	offered := map[string]bool{}
	for _, tool := range req.tools {
		offered[tool.Name] = true
	}
	for _, raw := range res.output {
		var it outputItem
		_ = json.Unmarshal(raw, &it)
		if it.Type != "function_call" {
			continue
		}
		namespaceOK := it.Namespace == ""
		if req.model.auth == AuthOpenAIChatGPT {
			namespaceOK = it.Namespace == "sam"
		}
		target := ""
		if namespaceOK && offered[it.Name] {
			target = it.Name
		}
		res.dispatch[it.CallID] = target
	}
}
