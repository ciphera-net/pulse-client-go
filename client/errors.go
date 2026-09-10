package client

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/ciphera-net/pulse-api-go/publicv1"
)

// Exit codes. These are a published contract — scripts branch on them — and map
// one-to-one onto the API's error.type.
const (
	ExitOK           = 0
	ExitServerError  = 1
	ExitInvalidInput = 2
	ExitUnauthorized = 3
	ExitNotFound     = 4
	ExitRateLimited  = 5
)

// APIError is a request that the API refused.
//
// Type and Code come from the response body when there is one. Status is always
// populated, because the body is not always ours: a bad credential arriving
// through the edge proxy returns an openresty HTML error page, and Bunny's WAF
// answers an SQLi-shaped query with its own 403 before the origin ever sees it.
// Both were observed against production. A client that decodes non-2xx bodies
// unconditionally reports "unknown error" for those, or panics on a nil field.
type APIError struct {
	Status  int
	Type    string
	Code    string
	Message string
	Param   string

	// RetryAfter is the parsed Retry-After header on a 429, zero otherwise.
	RetryAfter int

	// Body is what actually came back, kept for --json and for diagnosing the
	// responses that are not ours.
	Body []byte
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("HTTP %d %s", e.Status, http.StatusText(e.Status))
}

// ExitCode maps the error onto a process exit code.
//
// error.type is the branch, not error.code: type is the coarse, closed class
// and code is the specific reason, which is documented but open — new codes get
// added within an existing type, and a client keyed on code would exit 1 on a
// reason it already knows how to handle.
//
// An unrecognised type is a server error by the contract's own instruction ("a
// client may treat an unknown value as ErrorTypeServerError"), which is also the
// safe default: a script that stops is better than one that treats an unknown
// refusal as success.
func (e *APIError) ExitCode() int {
	switch e.Type {
	case publicv1.ErrorTypeInvalidRequest:
		return ExitInvalidInput
	case publicv1.ErrorTypeUnauthorized, publicv1.ErrorTypeForbidden:
		return ExitUnauthorized
	case publicv1.ErrorTypeNotFound:
		return ExitNotFound
	case publicv1.ErrorTypeRateLimited:
		return ExitRateLimited
	case publicv1.ErrorTypeServerError:
		return ExitServerError
	}

	// * No usable type — the body was not ours. Fall back to the status line,
	// * which is the one thing every intermediary sets correctly.
	switch {
	case e.Status == http.StatusTooManyRequests:
		return ExitRateLimited
	case e.Status == http.StatusUnauthorized, e.Status == http.StatusForbidden:
		return ExitUnauthorized
	case e.Status == http.StatusNotFound:
		return ExitNotFound
	case e.Status >= 400 && e.Status < 500:
		return ExitInvalidInput
	default:
		return ExitServerError
	}
}

// parseAPIError builds an APIError from a refused response.
//
// A body that does not decode is not an error here: it is recorded and the
// status line carries the meaning. Failing to parse an error is not a reason to
// report a different error than the one that happened.
func parseAPIError(status int, body []byte, header http.Header) *APIError {
	e := &APIError{Status: status, Body: body}

	var envelope publicv1.Error
	if err := json.Unmarshal(body, &envelope); err == nil && envelope.Error.Type != "" {
		e.Type = envelope.Error.Type
		e.Code = envelope.Error.Code
		e.Message = envelope.Error.Message
		e.Param = envelope.Error.Param
	} else {
		e.Message = fallbackMessage(status, body)
	}

	if status == http.StatusTooManyRequests {
		e.RetryAfter = parseRetryAfter(header.Get("Retry-After"))
	}
	return e
}

// fallbackMessage describes a refusal whose body we cannot read, without
// pasting an HTML page into the user's terminal.
func fallbackMessage(status int, body []byte) string {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" || strings.HasPrefix(trimmed, "<") {
		return fmt.Sprintf("HTTP %d %s (no error detail — the response did not come from the Pulse API; an edge proxy or WAF answered first)",
			status, http.StatusText(status))
	}
	const max = 200
	if len(trimmed) > max {
		trimmed = trimmed[:max] + "…"
	}
	return fmt.Sprintf("HTTP %d: %s", status, trimmed)
}
