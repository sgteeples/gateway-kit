// Package transform rewrites requests and responses as they pass through a
// route: header add/remove now, and the resolution of "$variables" in config
// values.
package transform

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/sgteeples/gateway-kit/internal/config"
)

// TimeFormat is used for $request_time and $response_time: RFC 3339, UTC,
// millisecond precision.
const TimeFormat = "2006-01-02T15:04:05.000Z07:00"

type requestTimeKey struct{}

// WithRequestTime records when the gateway received the request. It is set
// once at the edge so every transform on a request sees the same timestamp.
func WithRequestTime(ctx context.Context, t time.Time) context.Context {
	return context.WithValue(ctx, requestTimeKey{}, t)
}

// RequestTime returns the time recorded by WithRequestTime, or now.
func RequestTime(ctx context.Context) time.Time {
	if t, ok := ctx.Value(requestTimeKey{}).(time.Time); ok {
		return t
	}
	return time.Now()
}

// Vars are the values "$variables" resolve to for one request.
type Vars struct {
	RequestTime  time.Time
	ResponseTime time.Time // zero on the request side
	RoutePath    string
}

// String resolves a config value. Anything that isn't a known variable is
// returned unchanged; config validation has already rejected unknown ones.
func (v Vars) String(value string) string {
	switch {
	case strings.HasPrefix(value, config.LiteralPrefix):
		return strings.TrimPrefix(value, config.LiteralPrefix)
	case value == config.VarRequestTime:
		return v.RequestTime.UTC().Format(TimeFormat)
	case value == config.VarResponseTime:
		if v.ResponseTime.IsZero() {
			return time.Now().UTC().Format(TimeFormat)
		}
		return v.ResponseTime.UTC().Format(TimeFormat)
	case value == config.VarRoutePath:
		return v.RoutePath
	default:
		return value
	}
}

// applyHeaders removes first, then adds, so an add is never undone by a
// remove of the same name.
func applyHeaders(h http.Header, t *config.HeaderTransform, vars Vars) {
	for _, name := range t.Remove {
		h.Del(name)
	}
	for name, value := range t.Add {
		h.Set(name, vars.String(value))
	}
}

// RequestHeaders rewrites the incoming request's headers before it is forwarded.
func RequestHeaders(t *config.HeaderTransform, routePath string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			applyHeaders(r.Header, t, Vars{RequestTime: RequestTime(r.Context()), RoutePath: routePath})
			next.ServeHTTP(w, r)
		})
	}
}

// ResponseHeaders rewrites upstream response headers. It is a RoundTripper so
// it only ever sees real upstream responses, never the gateway's own errors.
func ResponseHeaders(t *config.HeaderTransform, routePath string, next http.RoundTripper) http.RoundTripper {
	return roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		resp, err := next.RoundTrip(req)
		if err != nil {
			return nil, err
		}
		applyHeaders(resp.Header, t, Vars{
			RequestTime:  RequestTime(req.Context()),
			ResponseTime: time.Now(),
			RoutePath:    routePath,
		})
		return resp, nil
	})
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
