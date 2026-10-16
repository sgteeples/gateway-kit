package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sgteeples/gateway-kit/internal/config"
	"github.com/sgteeples/gateway-kit/internal/mockupstream"
)

// harness wires a gateway to in-process mock upstreams. Tests write their own
// YAML with the upstreams' real (random) URLs, which also proves nothing is
// tied to the example config.
type harness struct {
	t       *testing.T
	gateway *httptest.Server
}

func newUpstream(t *testing.T, name string) (*mockupstream.Server, string) {
	t.Helper()
	m := mockupstream.New(name)
	srv := httptest.NewServer(m)
	t.Cleanup(srv.Close)
	return m, srv.URL
}

// deadURL is an address nothing is listening on.
func deadURL(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	return url
}

func newHarness(t *testing.T, yamlFormat string, args ...any) *harness {
	t.Helper()
	cfg, err := config.Parse([]byte(fmt.Sprintf(yamlFormat, args...)))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	gw, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("gateway: %v", err)
	}
	t.Cleanup(gw.Close)
	srv := httptest.NewServer(gw)
	t.Cleanup(srv.Close)
	return &harness{t: t, gateway: srv}
}

func (h *harness) do(method, path string, body io.Reader, header http.Header) *http.Response {
	h.t.Helper()
	req, err := http.NewRequest(method, h.gateway.URL+path, body)
	if err != nil {
		h.t.Fatal(err)
	}
	for k, vv := range header {
		req.Header[k] = vv
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func (h *harness) get(path string) *http.Response { return h.do("GET", path, nil, nil) }

func decode[T any](t *testing.T, resp *http.Response) T {
	t.Helper()
	var v T
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	return v
}

func wantStatus(t *testing.T, resp *http.Response, want int) {
	t.Helper()
	if resp.StatusCode != want {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want %d (body: %s)", resp.StatusCode, want, body)
	}
}

func TestHealth(t *testing.T) {
	_, up := newUpstream(t, "a")
	h := newHarness(t, `routes: [{path: "/", upstream: {url: %q}}]`, up)

	resp := h.get("/health")
	wantStatus(t, resp, http.StatusOK)
	body := decode[map[string]any](t, resp)
	if body["status"] != "healthy" {
		t.Errorf("status = %v", body["status"])
	}
	if _, ok := body["uptime_seconds"].(float64); !ok {
		t.Errorf("uptime_seconds missing or not a number: %v", body)
	}
}

func TestProxiesToMatchingRoute(t *testing.T) {
	_, users := newUpstream(t, "users")
	_, orders := newUpstream(t, "orders")
	h := newHarness(t, `
routes:
  - path: /api/users
    methods: [GET, POST]
    upstream: {url: %q}
  - path: /api/orders
    upstream: {url: %q}
`, users, orders)

	resp := h.get("/api/users/42?expand=true")
	wantStatus(t, resp, http.StatusOK)
	echo := decode[mockupstream.Echo](t, resp)
	if echo.Upstream != "users" || echo.Path != "/api/users/42" || echo.Query != "expand=true" {
		t.Errorf("unexpected echo: %+v", echo)
	}
	if resp.Header.Get("X-Request-ID") == "" {
		t.Error("response should carry an X-Request-ID")
	}

	echo = decode[mockupstream.Echo](t, h.get("/api/orders"))
	if echo.Upstream != "orders" {
		t.Errorf("/api/orders went to %s", echo.Upstream)
	}
}

func TestForwardsBodyAndHeaders(t *testing.T) {
	_, up := newUpstream(t, "a")
	h := newHarness(t, `routes: [{path: /api, upstream: {url: %q}}]`, up)

	resp := h.do("POST", "/api/things", strings.NewReader(`{"name":"widget"}`), http.Header{
		"Content-Type": {"application/json"},
		"X-Custom":     {"kept"},
	})
	wantStatus(t, resp, http.StatusOK)
	echo := decode[mockupstream.Echo](t, resp)
	if echo.Method != "POST" || string(echo.Body) != `{"name":"widget"}` {
		t.Errorf("method/body not forwarded: %+v", echo)
	}
	hdr := http.Header(echo.Headers)
	if hdr.Get("X-Custom") != "kept" {
		t.Error("custom header not forwarded")
	}
	if hdr.Get("X-Forwarded-For") == "" || hdr.Get("X-Request-Id") == "" {
		t.Errorf("forwarding headers missing: %v", hdr)
	}
}

func TestStripPrefix(t *testing.T) {
	_, up := newUpstream(t, "products")
	h := newHarness(t, `routes: [{path: /api/products, strip_prefix: true, upstream: {url: %q}}]`, up)

	if p := decode[mockupstream.Echo](t, h.get("/api/products/123")).Path; p != "/123" {
		t.Errorf("forwarded path = %q, want /123", p)
	}
	if p := decode[mockupstream.Echo](t, h.get("/api/products")).Path; p != "/" {
		t.Errorf("forwarded path = %q, want /", p)
	}
}

func TestLongestPrefixWins(t *testing.T) {
	_, general := newUpstream(t, "general")
	_, specific := newUpstream(t, "specific")
	h := newHarness(t, `
routes:
  - {path: /api, upstream: {url: %q}}
  - {path: /api/users, upstream: {url: %q}}
`, general, specific)

	if u := decode[mockupstream.Echo](t, h.get("/api/users/1")).Upstream; u != "specific" {
		t.Errorf("/api/users/1 went to %s", u)
	}
	if u := decode[mockupstream.Echo](t, h.get("/api/usersX")).Upstream; u != "general" {
		t.Errorf("/api/usersX went to %s; prefix must match on segment boundaries", u)
	}
}

func TestNotFoundAndMethodNotAllowed(t *testing.T) {
	_, up := newUpstream(t, "a")
	h := newHarness(t, `routes: [{path: /api/products, methods: [GET], upstream: {url: %q}}]`, up)

	resp := h.get("/nope")
	wantStatus(t, resp, http.StatusNotFound)
	if decode[map[string]any](t, resp)["error"] != "not_found" {
		t.Error("404 should have a JSON error body")
	}

	resp = h.do("POST", "/api/products", nil, nil)
	wantStatus(t, resp, http.StatusMethodNotAllowed)
	if got := resp.Header.Get("Allow"); got != "GET" {
		t.Errorf("Allow = %q, want GET", got)
	}
}

func TestDotSegmentsResolvedBeforeRouting(t *testing.T) {
	_, up := newUpstream(t, "a")
	h := newHarness(t, `routes: [{path: /public, upstream: {url: %q}}]`, up)

	// Must not reach the /public upstream as "/public/../secret".
	wantStatus(t, h.get("/public/../secret"), http.StatusNotFound)
}

func TestUpstreamDownIs502(t *testing.T) {
	h := newHarness(t, `routes: [{path: /, upstream: {url: %q}}]`, deadURL(t))

	resp := h.get("/anything")
	wantStatus(t, resp, http.StatusBadGateway)
	if decode[map[string]any](t, resp)["error"] != "bad_gateway" {
		t.Error("502 should have a JSON error body")
	}
}

func TestUpstreamTimeoutIs504(t *testing.T) {
	_, up := newUpstream(t, "slow")
	h := newHarness(t, `
gateway: {global_timeout: "5s"}
routes: [{path: /, upstream: {url: %q, timeout: "50ms"}}]
`, up)

	wantStatus(t, h.get("/slow?delay=2s"), http.StatusGatewayTimeout)
}

func TestUpstreamErrorStatusPassesThrough(t *testing.T) {
	_, up := newUpstream(t, "a")
	h := newHarness(t, `routes: [{path: /, upstream: {url: %q}}]`, up)

	// A 503 from the upstream is the upstream's answer, not a gateway failure.
	resp := h.get("/x?status=503")
	wantStatus(t, resp, http.StatusServiceUnavailable)
	if resp.Header.Get("Server") != "mockupstream/1.0" {
		t.Error("upstream response headers should be passed through")
	}
}

func TestLoadBalancesAcrossTargets(t *testing.T) {
	_, a := newUpstream(t, "a")
	_, b := newUpstream(t, "b")
	h := newHarness(t, `
routes:
  - path: /
    upstream:
      balance: weighted_round_robin
      targets: [{url: %q, weight: 3}, {url: %q, weight: 1}]
`, a, b)

	counts := map[string]int{}
	for range 8 {
		counts[decode[mockupstream.Echo](t, h.get("/")).Upstream]++
	}
	if counts["a"] != 6 || counts["b"] != 2 {
		t.Errorf("distribution = %v, want a:6 b:2", counts)
	}
}

func TestCleanPath(t *testing.T) {
	tests := map[string]string{
		"":                  "/",
		"/":                 "/",
		"/a/b/":             "/a/b/",
		"/public/../secret": "/secret",
		"/a/./b//c":         "/a/b/c",
		"/../../etc/passwd": "/etc/passwd",
	}
	for in, want := range tests {
		if got := cleanPath(in); got != want {
			t.Errorf("cleanPath(%q) = %q, want %q", in, got, want)
		}
	}
}
