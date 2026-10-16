// Package middleware holds client-facing policies: http.Handler decorators
// that can reject a request before anything is sent upstream.
package middleware

import (
	"crypto/subtle"
	"errors"
	"io"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sgteeples/gateway-kit/internal/config"
	"github.com/sgteeples/gateway-kit/internal/httperr"
	"github.com/sgteeples/gateway-kit/internal/netx"
	"github.com/sgteeples/gateway-kit/internal/ratelimit"
)

// Middleware wraps a handler with one policy.
type Middleware func(http.Handler) http.Handler

// globalKey is the single bucket used by `per: global` limits.
const globalKey = "*"

// RateLimit rejects requests over the limit with 429 and a Retry-After.
func RateLimit(l *ratelimit.Limiter, per string) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := globalKey
			if per == config.PerIP {
				key = netx.ClientIP(r)
			}
			if ok, retryAfter := l.Allow(key); !ok {
				httperr.Write(w, &httperr.Error{
					Status:     http.StatusTooManyRequests,
					Code:       "rate_limited",
					Message:    "rate limit exceeded",
					RetryAfter: retryAfter,
				})
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// APIKey requires one of keys in the named header. The key is removed before
// forwarding so upstreams never see gateway credentials.
func APIKey(header string, keys []string) Middleware {
	valid := make([][]byte, len(keys))
	for i, k := range keys {
		valid[i] = []byte(k)
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got := r.Header.Get(header)
			if got == "" {
				httperr.Write(w, &httperr.Error{Status: http.StatusUnauthorized, Code: "unauthorized", Message: "missing " + header + " header"})
				return
			}
			if !keyMatches([]byte(got), valid) {
				httperr.Write(w, &httperr.Error{Status: http.StatusUnauthorized, Code: "unauthorized", Message: "invalid API key"})
				return
			}
			r.Header.Del(header)
			next.ServeHTTP(w, r)
		})
	}
}

// keyMatches compares against every key in constant time, without stopping
// early, so response timing doesn't reveal how close a guess was.
func keyMatches(got []byte, valid [][]byte) bool {
	match := 0
	for _, k := range valid {
		match |= subtle.ConstantTimeCompare(got, k)
	}
	return match == 1
}

// errBodyTimeout is returned by a request body whose client stopped sending.
var errBodyTimeout = &httperr.Error{Status: http.StatusRequestTimeout, Code: "request_timeout", Message: "request body was not received in time"}

// BodyReadTimeout bounds how long a client may take to send its request body.
//
// Without it, nothing limits the body: the server only times out reading
// headers, and the route timeout starts later, when the upstream call does. A
// client trickling one byte a second would hold the request, its goroutine,
// and up to MaxBodyBytes of buffer forever (slow-POST DoS).
//
// The deadline is cleared once the body has been read, so it never cuts
// short the upstream call or its retries, which have their own timeouts.
func BodyReadTimeout(d time.Duration) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Body == nil || r.Body == http.NoBody {
				next.ServeHTTP(w, r)
				return
			}
			rc := http.NewResponseController(w)
			if err := rc.SetReadDeadline(time.Now().Add(d)); err != nil {
				next.ServeHTTP(w, r) // not a real connection (e.g. a test recorder)
				return
			}
			body := &deadlineBody{ReadCloser: r.Body, rc: rc}
			r.Body = body
			tw := &writeTracker{ResponseWriter: w}
			next.ServeHTTP(tw, r)

			// A body read timeout also makes the server cancel the request
			// context, so whichever layer was reading may have given up
			// with "context canceled" rather than the 408. Answer it here.
			if body.timedOut.Load() && !tw.wrote {
				httperr.Write(w, errBodyTimeout)
			}
		})
	}
}

type deadlineBody struct {
	io.ReadCloser
	rc       *http.ResponseController
	clear    sync.Once
	timedOut atomic.Bool
}

// Read lifts the deadline only once the whole body has arrived. A body that
// is closed early, or that timed out, keeps it: after the handler returns, the
// server drains any unread body to reuse the connection, and that drain must
// stay bounded too, or a slow client would hang it instead.
func (b *deadlineBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	switch {
	case err == io.EOF:
		// The transport may read the body from its own goroutine, hence Once.
		b.clear.Do(func() { _ = b.rc.SetReadDeadline(time.Time{}) })
	case errors.Is(err, os.ErrDeadlineExceeded):
		b.timedOut.Store(true)
		err = errBodyTimeout
	}
	return n, err
}

// writeTracker records whether a response has been started.
type writeTracker struct {
	http.ResponseWriter
	wrote bool
}

func (t *writeTracker) WriteHeader(code int) {
	t.wrote = true
	t.ResponseWriter.WriteHeader(code)
}

func (t *writeTracker) Write(b []byte) (int, error) {
	t.wrote = true
	return t.ResponseWriter.Write(b)
}

func (t *writeTracker) Unwrap() http.ResponseWriter { return t.ResponseWriter }
