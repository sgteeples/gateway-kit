package gateway

import (
	"compress/gzip"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sgteeples/gateway-kit/internal/mockupstream"
	"github.com/sgteeples/gateway-kit/internal/transform"
)

// legacyRoute is the /api/legacy route from the spec's gateway.yaml.
const legacyRoute = `
routes:
  - path: /api/legacy
    methods: [GET, POST]
    strip_prefix: true
    upstream: {url: %q}
    request_transform:
      headers:
        add: {X-Gateway: gatewaykit, X-Request-Start: $request_time}
        remove: [X-Debug, X-Internal]
      body:
        mapping:
          user.id: userId
          user.name: userName
          meta.source: $literal:gateway
          meta.timestamp: $request_time
    response_transform:
      headers:
        add: {X-Served-By: gatewaykit}
        remove: [Server, X-Powered-By]
      body:
        envelope:
          data: $body
          gateway_metadata:
            served_at: $response_time
            route: $route_path
`

// enveloped is the response shape the legacy route produces.
type enveloped struct {
	Data            mockupstream.Echo `json:"data"`
	GatewayMetadata struct {
		ServedAt string `json:"served_at"`
		Route    string `json:"route"`
	} `json:"gateway_metadata"`
}

func TestLegacyRouteFromSpec(t *testing.T) {
	_, up := newUpstream(t, "legacy")
	h := newHarness(t, legacyRoute, up)

	resp := h.do("POST", "/api/legacy/v1/data",
		strings.NewReader(`{"userId": 42, "userName": "ada", "debug": true}`),
		http.Header{"Content-Type": {"application/json"}, "X-Debug": {"1"}})
	wantStatus(t, resp, http.StatusOK)

	// Response: enveloped, with header rules applied.
	if resp.Header.Get("X-Served-By") != "gatewaykit" || resp.Header.Get("Server") != "" {
		t.Errorf("response headers not transformed: %v", resp.Header)
	}
	if resp.Header.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type = %q", resp.Header.Get("Content-Type"))
	}
	env := decode[enveloped](t, resp)
	if env.GatewayMetadata.Route != "/api/legacy" {
		t.Errorf("route = %q", env.GatewayMetadata.Route)
	}
	if _, err := time.Parse(transform.TimeFormat, env.GatewayMetadata.ServedAt); err != nil {
		t.Errorf("served_at = %q is not a timestamp", env.GatewayMetadata.ServedAt)
	}

	// What the upstream received, as echoed inside the envelope.
	echo := env.Data
	if echo.Path != "/v1/data" {
		t.Errorf("upstream path = %q, want /v1/data (strip_prefix)", echo.Path)
	}
	var sent struct {
		User struct {
			ID   int    `json:"id"`
			Name string `json:"name"`
		} `json:"user"`
		Meta struct {
			Source    string `json:"source"`
			Timestamp string `json:"timestamp"`
		} `json:"meta"`
		Debug *bool `json:"debug"`
	}
	if err := json.Unmarshal(echo.Body, &sent); err != nil {
		t.Fatalf("upstream body %s: %v", echo.Body, err)
	}
	if sent.User.ID != 42 || sent.User.Name != "ada" || sent.Meta.Source != "gateway" {
		t.Errorf("mapped body = %s", echo.Body)
	}
	if sent.Debug != nil {
		t.Error("unmapped fields must not be forwarded")
	}
	hdr := http.Header(echo.Headers)
	if sent.Meta.Timestamp != hdr.Get("X-Request-Start") {
		t.Errorf("body timestamp %q and X-Request-Start %q should be the same instant", sent.Meta.Timestamp, hdr.Get("X-Request-Start"))
	}
	if cl := hdr.Get("Content-Length"); cl != strconv.Itoa(len(echo.Body)) {
		t.Errorf("upstream Content-Length = %s, body is %d bytes", cl, len(echo.Body))
	}
}

func TestRequestMappingPassThrough(t *testing.T) {
	_, up := newUpstream(t, "legacy")
	h := newHarness(t, legacyRoute, up)

	// A non-JSON body is forwarded untouched.
	resp := h.do("POST", "/api/legacy/x", strings.NewReader("a=1&b=2"), http.Header{"Content-Type": {"application/x-www-form-urlencoded"}})
	wantStatus(t, resp, http.StatusOK)
	var got string
	if err := json.Unmarshal(decode[enveloped](t, resp).Data.Body, &got); err != nil || got != "a=1&b=2" {
		t.Errorf("form body was changed: %q (%v)", got, err)
	}

	// A GET with no body still works.
	wantStatus(t, h.get("/api/legacy/x"), http.StatusOK)
}

func TestRequestMappingWithoutJSONContentType(t *testing.T) {
	_, up := newUpstream(t, "legacy")
	h := newHarness(t, legacyRoute, up)

	// What `curl -d '{...}'` sends: JSON labelled as a form. And no header at all.
	for _, ct := range []string{"application/x-www-form-urlencoded", ""} {
		t.Run("content-type="+ct, func(t *testing.T) {
			hdr := http.Header{}
			if ct != "" {
				hdr.Set("Content-Type", ct)
			}
			resp := h.do("POST", "/api/legacy/x", strings.NewReader(`{"userId": 7, "userName": "grace"}`), hdr)
			wantStatus(t, resp, http.StatusOK)
			echo := decode[enveloped](t, resp).Data

			var sent struct {
				User struct {
					ID   int    `json:"id"`
					Name string `json:"name"`
				} `json:"user"`
			}
			if err := json.Unmarshal(echo.Body, &sent); err != nil || sent.User.ID != 7 || sent.User.Name != "grace" {
				t.Errorf("body was not mapped: %s", echo.Body)
			}
			if got := http.Header(echo.Headers).Get("Content-Type"); got != "application/json" {
				t.Errorf("upstream Content-Type = %q, want application/json", got)
			}
		})
	}

	// JSON that isn't an object (here a bare number) is not a mapping source.
	resp := h.do("POST", "/api/legacy/x", strings.NewReader(`42`), http.Header{"Content-Type": {"text/plain"}})
	wantStatus(t, resp, http.StatusOK)
	if got := string(decode[enveloped](t, resp).Data.Body); got != "42" {
		t.Errorf("non-object body should pass through unchanged, upstream got %s", got)
	}
}

func TestRequestMappingRejectsInvalidJSON(t *testing.T) {
	up, upURL := newUpstream(t, "legacy")
	h := newHarness(t, legacyRoute, upURL)

	resp := h.do("POST", "/api/legacy/x", strings.NewReader(`{"userId": 42,`), http.Header{"Content-Type": {"application/json"}})
	wantStatus(t, resp, http.StatusBadRequest)
	if decode[map[string]any](t, resp)["error"] != "invalid_json" {
		t.Error("want an invalid_json error")
	}
	if up.Requests() != 0 {
		t.Error("an invalid body must not be forwarded")
	}
}

func TestEnvelopeWrapsUpstreamErrorsKeepingStatus(t *testing.T) {
	_, up := newUpstream(t, "legacy")
	h := newHarness(t, legacyRoute, up)

	resp := h.get("/api/legacy/x?status=404")
	wantStatus(t, resp, http.StatusNotFound)
	if env := decode[enveloped](t, resp); env.GatewayMetadata.Route != "/api/legacy" {
		t.Error("upstream error responses should be enveloped too")
	}
}

func TestEnvelopeSkipsGatewayErrors(t *testing.T) {
	h := newHarness(t, legacyRoute, deadURL(t))
	resp := h.get("/api/legacy/x")
	wantStatus(t, resp, http.StatusBadGateway)
	if body := decode[map[string]any](t, resp); body["error"] != "bad_gateway" {
		t.Errorf("gateway errors must not be wrapped: %v", body)
	}
}

func TestEnvelopeHandlesGzipUpstream(t *testing.T) {
	// An upstream that gzips whenever the client allows it.
	gz := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			w.Header().Set("Content-Encoding", "gzip")
			zw := gzip.NewWriter(w)
			zw.Write([]byte(`{"compressed": true}`))
			zw.Close()
			return
		}
		w.Write([]byte(`{"compressed": true}`))
	}))
	t.Cleanup(gz.Close)

	h := newHarness(t, `
routes:
  - path: /
    upstream: {url: %q}
    response_transform: {body: {envelope: {data: $body}}}
`, gz.URL)

	// The client advertises gzip; the gateway must still parse and wrap the body.
	resp := h.do("GET", "/", nil, http.Header{"Accept-Encoding": {"gzip"}})
	wantStatus(t, resp, http.StatusOK)
	var body struct {
		Data struct {
			Compressed bool `json:"compressed"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || !body.Data.Compressed {
		t.Errorf("gzip body not unwrapped and enveloped: %+v, %v", body, err)
	}
}

func TestHealthChecksSteerTraffic(t *testing.T) {
	sick, sickURL := newUpstream(t, "sick")
	_, wellURL := newUpstream(t, "well")
	h := newHarness(t, `
routes:
  - path: /
    upstream:
      balance: round_robin
      targets: [{url: %q}, {url: %q}]
    health_check: {path: /healthz, interval: "100ms", unhealthy_threshold: 2}
`, sickURL, wellURL)

	sick.SetHealthy(false)
	eventually(t, func() bool {
		// Once the checker marks it down, every request goes to "well".
		for range 4 {
			if decode[mockupstream.Echo](t, h.get("/")).Upstream != "well" {
				return false
			}
		}
		return true
	})

	sick.SetHealthy(true)
	eventually(t, func() bool {
		// Round robin alternates, so two requests are enough to reach "sick".
		for range 2 {
			if decode[mockupstream.Echo](t, h.get("/")).Upstream == "sick" {
				return true
			}
		}
		return false
	})
}

// eventually polls cond until it holds or 2s pass.
func eventually(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 2s")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
