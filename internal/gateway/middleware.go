package gateway

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"path"
	"strings"
	"time"

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

// accessLog cleans the request path, assigns a request ID that is forwarded
// upstream and echoed back, and logs one line per request.
func (g *Gateway) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		r = r.WithContext(transform.WithRequestTime(r.Context(), start))

		// Resolve "." and ".." before routing, so "/api/public/../internal"
		// is matched (and authorized) as "/api/internal".
		r.URL.Path = cleanPath(r.URL.Path)
		r.URL.RawPath = ""

		id := r.Header.Get("X-Request-ID")
		if id == "" {
			id = newRequestID()
			r.Header.Set("X-Request-ID", id)
		}
		w.Header().Set("X-Request-ID", id)

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		g.logger.Info("request",
			"id", id,
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
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
