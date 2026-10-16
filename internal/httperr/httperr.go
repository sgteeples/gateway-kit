// Package httperr gives every layer of the gateway one way to describe and
// write an error response, so clients always see the same JSON shape:
//
//	{"error": "<code>", "message": "...", "retry_after": <seconds>}
//
// Upstream-facing code returns *Error values as ordinary Go errors; the proxy
// handler is the single place that turns them into responses.
package httperr

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strconv"
	"time"
)

type Error struct {
	Status     int
	Code       string        // machine-readable, e.g. "rate_limited"
	Message    string        // optional human-readable detail
	RetryAfter time.Duration // if > 0, sent as a Retry-After header and retry_after field
	Err        error         // underlying cause; logged, never sent to the client
}

func (e *Error) Error() string {
	if e.Err != nil {
		return e.Code + ": " + e.Err.Error()
	}
	return e.Code
}

func (e *Error) Unwrap() error { return e.Err }

// From classifies an error from an upstream call. Errors that are already an
// *Error pass through; timeouts become 504; anything else is a 502.
func From(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &Error{Status: http.StatusGatewayTimeout, Code: "gateway_timeout", Message: "upstream did not respond in time", Err: err}
	}
	return &Error{Status: http.StatusBadGateway, Code: "bad_gateway", Message: "upstream request failed", Err: err}
}

// Write sends e as a JSON error response.
func Write(w http.ResponseWriter, e *Error) {
	body := map[string]any{"error": e.Code}
	if e.Message != "" {
		body["message"] = e.Message
	}
	if e.RetryAfter > 0 {
		secs := int(math.Ceil(e.RetryAfter.Seconds()))
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		body["retry_after"] = secs
	}
	WriteJSON(w, e.Status, body)
}

// WriteJSON writes v as a JSON response with the given status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Del("Content-Length")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
