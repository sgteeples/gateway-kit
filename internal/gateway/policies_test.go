package gateway

import (
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/sgteeples/gateway-kit/internal/mockupstream"
	"github.com/sgteeples/gateway-kit/internal/transform"
)

// TestRateLimitUnderConcurrency fires 50 simultaneous requests at a route
// limited to 10: exactly 10 must reach the upstream and 40 must get a 429.
func TestRateLimitUnderConcurrency(t *testing.T) {
	for _, strategy := range []string{"fixed_window", "sliding_window"} {
		t.Run(strategy, func(t *testing.T) {
			up, upURL := newUpstream(t, "a")
			h := newHarness(t, `
routes:
  - path: /limited
    upstream: {url: %q}
    rate_limit: {requests: 10, window: "1m", strategy: %s, per: ip}
`, upURL, strategy)

			statuses := make(chan int, 50)
			var wg sync.WaitGroup
			start := make(chan struct{})
			for range 50 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					resp, err := http.Get(h.gateway.URL + "/limited")
					if err != nil {
						t.Error(err)
						return
					}
					resp.Body.Close()
					statuses <- resp.StatusCode
				}()
			}
			close(start)
			wg.Wait()
			close(statuses)

			counts := map[int]int{}
			for s := range statuses {
				counts[s]++
			}
			if counts[200] != 10 || counts[429] != 40 {
				t.Errorf("statuses = %v, want 10x200 and 40x429", counts)
			}
			if got := up.Requests(); got != 10 {
				t.Errorf("upstream saw %d requests, want 10", got)
			}
		})
	}
}

func TestRateLimitResponse(t *testing.T) {
	_, up := newUpstream(t, "a")
	h := newHarness(t, `routes: [{path: /, upstream: {url: %q}, rate_limit: {requests: 1, window: "30s"}}]`, up)

	wantStatus(t, h.get("/"), http.StatusOK)
	resp := h.get("/")
	wantStatus(t, resp, http.StatusTooManyRequests)

	retry, err := strconv.Atoi(resp.Header.Get("Retry-After"))
	if err != nil || retry < 1 || retry > 30 {
		t.Errorf("Retry-After = %q, want 1..30 seconds", resp.Header.Get("Retry-After"))
	}
	body := decode[map[string]any](t, resp)
	if body["error"] != "rate_limited" || body["retry_after"] != float64(retry) {
		t.Errorf("body = %v", body)
	}
}

func TestGlobalRateLimitAppliesPerRoute(t *testing.T) {
	_, up := newUpstream(t, "a")
	h := newHarness(t, `
gateway:
  global_rate_limit: {requests: 2, window: "1m"}
routes:
  - {path: /a, upstream: {url: %q}}
  - {path: /b, upstream: {url: %[1]q}}
  - {path: /c, upstream: {url: %[1]q}, rate_limit: {requests: 5, window: "1m"}}
`, up)

	for range 2 {
		wantStatus(t, h.get("/a"), http.StatusOK)
	}
	wantStatus(t, h.get("/a"), http.StatusTooManyRequests)
	// /b inherits the same default but has its own buckets.
	wantStatus(t, h.get("/b"), http.StatusOK)
	// /c overrides the default.
	for range 5 {
		wantStatus(t, h.get("/c"), http.StatusOK)
	}
}

func TestAPIKeyAuth(t *testing.T) {
	_, up := newUpstream(t, "internal")
	h := newHarness(t, `
routes:
  - path: /api/internal
    upstream: {url: %q}
    auth: {type: api_key, header: X-API-Key, keys: [sk_one, sk_two]}
`, up)

	wantStatus(t, h.get("/api/internal"), http.StatusUnauthorized)
	wantStatus(t, h.do("GET", "/api/internal", nil, http.Header{"X-Api-Key": {"sk_wrong"}}), http.StatusUnauthorized)

	resp := h.do("GET", "/api/internal", nil, http.Header{"X-Api-Key": {"sk_two"}})
	wantStatus(t, resp, http.StatusOK)
	echo := decode[mockupstream.Echo](t, resp)
	if _, leaked := echo.Headers["X-Api-Key"]; leaked {
		t.Error("API key was forwarded to the upstream")
	}
}

func TestAuthRunsBeforeRateLimit(t *testing.T) {
	_, up := newUpstream(t, "a")
	h := newHarness(t, `
routes:
  - path: /
    upstream: {url: %q}
    auth: {type: api_key, header: X-API-Key, keys: [k]}
    rate_limit: {requests: 1, window: "1m", per: global}
`, up)

	// Unauthenticated noise must not use up the shared quota.
	for range 3 {
		wantStatus(t, h.get("/"), http.StatusUnauthorized)
	}
	wantStatus(t, h.do("GET", "/", nil, http.Header{"X-Api-Key": {"k"}}), http.StatusOK)
}

func TestHeaderTransforms(t *testing.T) {
	_, up := newUpstream(t, "legacy")
	h := newHarness(t, `
routes:
  - path: /api/legacy
    strip_prefix: true
    upstream: {url: %q}
    request_transform:
      headers:
        add: {X-Gateway: gatewaykit, X-Request-Start: $request_time, X-Route: $route_path}
        remove: [X-Debug, X-Internal]
    response_transform:
      headers:
        add: {X-Served-By: gatewaykit}
        remove: [Server, X-Powered-By]
`, up)

	before := time.Now().Add(-time.Second)
	resp := h.do("GET", "/api/legacy/v1/data", nil, http.Header{
		"X-Debug":    {"1"},
		"X-Internal": {"secret"},
		"X-Keep":     {"yes"},
	})
	wantStatus(t, resp, http.StatusOK)

	// Response side.
	if resp.Header.Get("X-Served-By") != "gatewaykit" {
		t.Error("X-Served-By not added to response")
	}
	if resp.Header.Get("Server") != "" || resp.Header.Get("X-Powered-By") != "" {
		t.Errorf("upstream identity headers not removed: Server=%q X-Powered-By=%q",
			resp.Header.Get("Server"), resp.Header.Get("X-Powered-By"))
	}

	// Request side, as seen by the upstream.
	got := http.Header(decode[mockupstream.Echo](t, resp).Headers)
	if got.Get("X-Gateway") != "gatewaykit" || got.Get("X-Route") != "/api/legacy" {
		t.Errorf("added headers missing: %v", got)
	}
	if got.Get("X-Debug") != "" || got.Get("X-Internal") != "" {
		t.Error("removed headers reached the upstream")
	}
	if got.Get("X-Keep") != "yes" {
		t.Error("unrelated header was dropped")
	}
	start, err := time.Parse(transform.TimeFormat, got.Get("X-Request-Start"))
	if err != nil || start.Before(before) || start.After(time.Now()) {
		t.Errorf("X-Request-Start = %q, want a current RFC 3339 timestamp", got.Get("X-Request-Start"))
	}
}

func TestResponseTransformSkipsGatewayErrors(t *testing.T) {
	h := newHarness(t, `
routes:
  - path: /
    upstream: {url: %q}
    response_transform: {headers: {add: {X-Served-By: gatewaykit}}}
`, deadURL(t))

	resp := h.get("/")
	wantStatus(t, resp, http.StatusBadGateway)
	if resp.Header.Get("X-Served-By") != "" {
		t.Error("response transform should only apply to upstream responses")
	}
}
