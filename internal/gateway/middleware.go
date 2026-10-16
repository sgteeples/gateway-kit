package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
	"unicode"

	"github.com/sgteeples/gateway-kit/internal/httperr"
	"github.com/sgteeples/gateway-kit/internal/transform"
)

// recoverPanics turns a panic in any route into a 500 instead of a dropped
// connection, and logs it.
func (g *Gateway) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if err, ok := v.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(v)
				}
				g.logger.Error("panic serving request", "path", r.URL.Path, "panic", v)
				httperr.Write(w, &httperr.Error{Status: http.StatusInternalServerError, Code: "internal_error"})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// accessLog assigns a request ID that is forwarded upstream and echoed back,
// records the request time, and logs one line per request.
func (g *Gateway) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		r = r.WithContext(transform.WithRequestTime(r.Context(), start))

		id := r.Header.Get("X-Request-ID")
		if id == "" {
			id = newRequestID()
			r.Header.Set("X-Request-ID", id)
		}
		w.Header().Set("X-Request-ID", id)

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		matched := &matchedRoute{}
		next.ServeHTTP(rec, r.WithContext(context.WithValue(r.Context(), matchedRouteKey{}, matched)))

		g.logger.Info("request",
			"id", id,
			"method", r.Method,
			"path", r.URL.Path,
			"route", matched.path,
			"status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

// normalizePath makes sure the path the router matches is the path the
// upstream will see, so a route's policies (auth especially) can't be
// sidestepped by a path that the gateway and upstream read differently:
//
//   - ambiguous paths are rejected with 400 (see ambiguousPath)
//   - "." and ".." are resolved, so "/public/../internal" is matched and
//     authorized as "/internal"
func (g *Gateway) normalizePath(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if reason := ambiguousPath(r.URL); reason != "" {
			httperr.Write(w, &httperr.Error{Status: http.StatusBadRequest, Code: "bad_request", Message: "ambiguous request path: " + reason})
			return
		}
		r.URL.Path = cleanPath(r.URL.Path)
		r.URL.RawPath = ""
		next.ServeHTTP(w, r)
	})
}

// ambiguousPath reports why a path could be read differently by the gateway
// and an upstream, or "" if it's fine.
//
//   - ";" starts path parameters in Java servers (Tomcat, Spring): the gateway
//     sees "/api/internal;x" (no match, so a broader route), the upstream
//     serves "/api/internal".
//   - Encoded "/" or "\" (also double-encoded) is one segment to the router
//     but may become a separator upstream.
//   - Control characters have no legitimate use in a path.
func ambiguousPath(u *url.URL) string {
	decoded := strings.ToLower(u.Path)
	escaped := strings.ToLower(u.EscapedPath())
	switch {
	case strings.Contains(decoded, ";"):
		return `contains ";"`
	case strings.Contains(decoded, `\`):
		return `contains "\"`
	case strings.Contains(escaped, "%2f"), strings.Contains(escaped, "%5c"),
		strings.Contains(decoded, "%2f"), strings.Contains(decoded, "%5c"):
		return "contains an encoded slash or backslash"
	case strings.IndexFunc(decoded, unicode.IsControl) >= 0:
		return "contains a control character"
	}
	return ""
}

type matchedRouteKey struct{}

// matchedRoute is filled in by the route's handler so the access log, which
// runs outside the router, can say which route served the request.
type matchedRoute struct{ path string }

func tagRoute(path string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if m, ok := r.Context().Value(matchedRouteKey{}).(*matchedRoute); ok {
			m.path = path
		}
		next.ServeHTTP(w, r)
	})
}

func cleanPath(p string) string {
	if p == "" {
		return "/"
	}
	cleaned := path.Clean(p)
	if strings.HasSuffix(p, "/") && cleaned != "/" {
		cleaned += "/"
	}
	return cleaned
}

func newRequestID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach the underlying writer (Flush etc).
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }
