package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sgteeples/gateway-kit/internal/config"
	"github.com/sgteeples/gateway-kit/internal/ratelimit"
)

var ok = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

func requestFrom(ip string) *http.Request {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = ip + ":12345"
	return r
}

func TestRateLimitKeys(t *testing.T) {
	for _, tt := range []struct {
		per          string
		wantSecondIP int
	}{
		{config.PerIP, http.StatusOK},                  // each IP has its own bucket
		{config.PerGlobal, http.StatusTooManyRequests}, // everyone shares one
	} {
		t.Run(tt.per, func(t *testing.T) {
			l := ratelimit.New(config.RateLimit{Requests: 1, Window: config.Duration(time.Minute), Strategy: config.StrategyFixedWindow}, time.Now)
			h := RateLimit(l, tt.per)(ok)

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, requestFrom("10.0.0.1"))
			if rec.Code != http.StatusOK {
				t.Fatalf("first request = %d", rec.Code)
			}

			rec = httptest.NewRecorder()
			h.ServeHTTP(rec, requestFrom("10.0.0.2"))
			if rec.Code != tt.wantSecondIP {
				t.Errorf("second IP = %d, want %d", rec.Code, tt.wantSecondIP)
			}
		})
	}
}

func TestRateLimitIgnoresXForwardedFor(t *testing.T) {
	l := ratelimit.New(config.RateLimit{Requests: 1, Window: config.Duration(time.Minute), Strategy: config.StrategyFixedWindow}, time.Now)
	h := RateLimit(l, config.PerIP)(ok)

	h.ServeHTTP(httptest.NewRecorder(), requestFrom("10.0.0.1"))
	r := requestFrom("10.0.0.1")
	r.Header.Set("X-Forwarded-For", "1.2.3.4") // spoofed to dodge the limit
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("spoofed X-Forwarded-For got a fresh bucket: %d", rec.Code)
	}
}

func TestAPIKey(t *testing.T) {
	var forwarded http.Header
	h := APIKey("X-API-Key", []string{"k1", "k2"})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded = r.Header.Clone()
	}))

	for _, tt := range []struct {
		name, key string
		want      int
	}{
		{"missing", "", http.StatusUnauthorized},
		{"wrong", "nope", http.StatusUnauthorized},
		{"prefix of a valid key", "k", http.StatusUnauthorized},
		{"first key", "k1", http.StatusOK},
		{"second key", "k2", http.StatusOK},
	} {
		t.Run(tt.name, func(t *testing.T) {
			forwarded = nil
			r := httptest.NewRequest("GET", "/", nil)
			if tt.key != "" {
				r.Header.Set("X-API-Key", tt.key)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, r)
			if rec.Code != tt.want {
				t.Errorf("status = %d, want %d", rec.Code, tt.want)
			}
			if tt.want == http.StatusOK && forwarded.Get("X-API-Key") != "" {
				t.Error("API key should be stripped before forwarding")
			}
		})
	}
}
