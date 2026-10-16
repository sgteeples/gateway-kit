// Package middleware holds client-facing policies: http.Handler decorators
// that can reject a request before anything is sent upstream.
package middleware

import (
	"crypto/subtle"
	"net/http"

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
