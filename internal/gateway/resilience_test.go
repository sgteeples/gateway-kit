package gateway

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sgteeples/gateway-kit/internal/mockupstream"
)

func TestCircuitBreakerOpensAndFailsFast(t *testing.T) {
	up, upURL := newUpstream(t, "internal")
	h := newHarness(t, `
routes:
  - path: /api/internal
    upstream: {url: %q}
    circuit_breaker: {threshold: 3, window: "60s", cooldown: "30s"}
`, upURL)

	up.FailNext(3, http.StatusInternalServerError)
	for range 3 {
		wantStatus(t, h.get("/api/internal"), http.StatusInternalServerError)
	}

	// The upstream is healthy again, but the breaker is open: fail fast.
	resp := h.get("/api/internal")
	wantStatus(t, resp, http.StatusServiceUnavailable)
	body := decode[map[string]any](t, resp)
	if len(body) != 2 || body["error"] != "service_unavailable" || body["retry_after"] != float64(30) {
		t.Errorf(`body = %v, want exactly {"error":"service_unavailable","retry_after":30}`, body)
	}
	if resp.Header.Get("Retry-After") != "30" {
		t.Errorf("Retry-After = %q, want 30", resp.Header.Get("Retry-After"))
	}
	if got := up.Requests(); got != 3 {
		t.Errorf("upstream saw %d requests; an open breaker must not forward any", got)
	}
}

func TestCircuitBreakerRecoversAfterCooldown(t *testing.T) {
	up, upURL := newUpstream(t, "internal")
	h := newHarness(t, `
routes:
  - path: /
    upstream: {url: %q}
    circuit_breaker: {threshold: 2, window: "60s", cooldown: "200ms"}
`, upURL)

	up.FailNext(2, http.StatusBadGateway)
	h.get("/")
	h.get("/")
	wantStatus(t, h.get("/"), http.StatusServiceUnavailable)

	time.Sleep(250 * time.Millisecond)
	wantStatus(t, h.get("/"), http.StatusOK) // the half-open probe succeeds
	wantStatus(t, h.get("/"), http.StatusOK) // closed again
}

func TestCircuitBreakerTripsOnUnreachableUpstream(t *testing.T) {
	h := newHarness(t, `
routes:
  - path: /
    upstream: {url: %q}
    circuit_breaker: {threshold: 2, window: "60s", cooldown: "30s"}
`, deadURL(t))

	wantStatus(t, h.get("/"), http.StatusBadGateway)
	wantStatus(t, h.get("/"), http.StatusBadGateway)
	wantStatus(t, h.get("/"), http.StatusServiceUnavailable)
}

func TestRetryRecoversAndReplaysBody(t *testing.T) {
	up, upURL := newUpstream(t, "orders")
	h := newHarness(t, `
routes:
  - path: /api/orders
    methods: [POST]
    upstream: {url: %q}
    retry: {attempts: 3, backoff: exponential, initial_delay: "10ms", on: [502, 503, 504]}
`, upURL)

	up.FailNext(2, http.StatusServiceUnavailable)
	resp := h.do("POST", "/api/orders", strings.NewReader(`{"item":"widget","qty":2}`), http.Header{"Content-Type": {"application/json"}})
	wantStatus(t, resp, http.StatusOK)
	if body := string(decode[mockupstream.Echo](t, resp).Body); body != `{"item":"widget","qty":2}` {
		t.Errorf("body on the successful attempt = %s", body)
	}
	if got := up.Requests(); got != 3 {
		t.Errorf("upstream saw %d requests, want 3", got)
	}
}

func TestRetryGivesUpWithUpstreamResponse(t *testing.T) {
	up, upURL := newUpstream(t, "orders")
	h := newHarness(t, `
routes:
  - path: /
    upstream: {url: %q}
    retry: {attempts: 3, initial_delay: "1ms", on: [503]}
`, upURL)

	up.FailNext(10, http.StatusServiceUnavailable)
	resp := h.get("/")
	wantStatus(t, resp, http.StatusServiceUnavailable)
	if decode[map[string]any](t, resp)["error"] != "injected failure" {
		t.Error("client should get the upstream's real last response, not a gateway error")
	}
	if got := up.Requests(); got != 3 {
		t.Errorf("upstream saw %d requests, want 3 (attempts is the total)", got)
	}
}

func TestRetryFailsOverToAnotherTarget(t *testing.T) {
	_, live := newUpstream(t, "live")
	h := newHarness(t, `
routes:
  - path: /
    upstream:
      balance: round_robin
      targets: [{url: %q}, {url: %q}]
    retry: {attempts: 2, initial_delay: "1ms", on: [502]}
`, deadURL(t), live)

	// Every other attempt hits the dead target. Because retry sits above
	// target selection, the retry lands on the live one.
	for range 6 {
		wantStatus(t, h.get("/"), http.StatusOK)
	}
}
