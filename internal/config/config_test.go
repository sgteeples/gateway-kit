package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadExampleConfig(t *testing.T) {
	cfg, err := Load("../../gateway.yaml")
	if err != nil {
		t.Fatalf("example config should load: %v", err)
	}
	if cfg.Gateway.Port != 8080 {
		t.Errorf("port = %d, want 8080", cfg.Gateway.Port)
	}
	if len(cfg.Routes) != 5 {
		t.Fatalf("got %d routes, want 5", len(cfg.Routes))
	}

	users := cfg.Routes[0]
	if users.Upstream.Timeout.Std() != 30*time.Second {
		t.Errorf("users timeout = %v, want global 30s", users.Upstream.Timeout.Std())
	}
	if users.RateLimit.Strategy != StrategySlidingWindow || users.RateLimit.Requests != 30 {
		t.Errorf("users rate limit should be its own override, got %+v", users.RateLimit)
	}

	products := cfg.Routes[2]
	if len(products.Upstream.Targets) != 2 || products.Upstream.Targets[0].Weight != 3 {
		t.Errorf("products targets = %+v", products.Upstream.Targets)
	}
	if products.RateLimit == nil || products.RateLimit.Requests != 100 {
		t.Errorf("products should inherit the global rate limit, got %+v", products.RateLimit)
	}

	legacy := cfg.Routes[3]
	if len(legacy.Upstream.Targets) != 1 || legacy.Upstream.Targets[0].URL != "http://localhost:3005" {
		t.Errorf("single url should normalize to one target, got %+v", legacy.Upstream.Targets)
	}
}

func TestDefaults(t *testing.T) {
	cfg, err := Parse([]byte(`
gateway:
  global_rate_limit: {requests: 5, window: "1s"}
routes:
  - path: "/a/"
    methods: [get]
    upstream: {url: "http://localhost:1"}
    retry: {attempts: 2}
  - path: "/b"
    upstream: {url: "http://localhost:2"}
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Gateway.Port != DefaultPort || cfg.Gateway.GlobalTimeout.Std() != DefaultGlobalTimeout {
		t.Errorf("gateway defaults not applied: %+v", cfg.Gateway)
	}
	a, b := cfg.Routes[0], cfg.Routes[1]
	if a.Path != "/a" {
		t.Errorf("trailing slash not trimmed: %q", a.Path)
	}
	if a.Methods[0] != "GET" {
		t.Errorf("methods not upper-cased: %v", a.Methods)
	}
	if a.Retry.Backoff != BackoffFixed || len(a.Retry.On) != 3 {
		t.Errorf("retry defaults not applied: %+v", a.Retry)
	}
	if a.RateLimit.Strategy != StrategyFixedWindow || a.RateLimit.Per != PerIP {
		t.Errorf("rate limit defaults not applied: %+v", a.RateLimit)
	}
	if a.RateLimit == b.RateLimit {
		t.Error("routes must get their own copy of the global rate limit")
	}
}

func TestValidationErrors(t *testing.T) {
	const route = `
routes:
  - path: "/x"
    upstream: {url: "http://localhost:1"}
`
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{"empty", ``, "config is empty"},
		{"no routes", `gateway: {port: 8080}`, "routes: at least one route is required"},
		{"unknown field", "gateway: {prot: 8080}\n" + route, "field prot not found"},
		{"bad duration", "gateway: {global_timeout: \"30 seconds\"}\n" + route, `invalid duration "30 seconds"`},
		{"bad port", "gateway: {port: 70000}\n" + route, "gateway.port"},
		{"path without slash", `routes: [{path: "x", upstream: {url: "http://h"}}]`, "routes[0].path"},
		{"bad method", `routes: [{path: "/x", methods: [FETCH], upstream: {url: "http://h"}}]`, `unknown HTTP method "FETCH"`},
		{"no upstream", `routes: [{path: "/x", upstream: {}}]`, "one of url or targets is required"},
		{"url and targets", `routes: [{path: "/x", upstream: {url: "http://h", targets: [{url: "http://h"}]}}]`, "mutually exclusive"},
		{"relative url", `routes: [{path: "/x", upstream: {url: "localhost:3001"}}]`, "absolute http(s) URL"},
		{"bad balance", `routes: [{path: "/x", upstream: {url: "http://h", balance: random}}]`, "balance: must be one of"},
		{"duplicate path", `routes: [{path: "/x", upstream: {url: "http://h"}}, {path: "/x/", upstream: {url: "http://h"}}]`, "duplicates routes[0].path"},
		{"bad strategy", `routes: [{path: "/x", upstream: {url: "http://h"}, rate_limit: {requests: 1, window: "1s", strategy: token_bucket}}]`, "strategy: must be one of"},
		{"zero window", `routes: [{path: "/x", upstream: {url: "http://h"}, rate_limit: {requests: 1}}]`, "rate_limit.window: is required"},
		{"bad retry code", `routes: [{path: "/x", upstream: {url: "http://h"}, retry: {attempts: 2, on: [700]}}]`, "not an HTTP status code: 700"},
		{"auth without keys", `routes: [{path: "/x", upstream: {url: "http://h"}, auth: {type: api_key, header: X-Key}}]`, "at least one key is required"},
		{"conflicting mapping", `routes: [{path: "/x", upstream: {url: "http://h"}, request_transform: {body: {mapping: {user: a, user.id: b}}}}]`, `"user" and "user.id" conflict`},
		{"retry attempts too high", `routes: [{path: "/x", upstream: {url: "http://h"}, retry: {attempts: 1000}}]`, "attempts: must be between 1 and 10"},
		{"retry delay too long", `routes: [{path: "/x", upstream: {url: "http://h"}, retry: {attempts: 2, initial_delay: "5m"}}]`, "initial_delay: must be at most 30s"},
		{"weight too high", `routes: [{path: "/x", upstream: {targets: [{url: "http://h", weight: 5000}]}}]`, "weight: must be between 0 and 1000"},
		{"breaker threshold too high", `routes: [{path: "/x", upstream: {url: "http://h"}, circuit_breaker: {threshold: 2000000000, window: "1s", cooldown: "1s"}}]`, "threshold: must be between 1 and 10000"},
		{"health interval too short", `routes: [{path: "/x", upstream: {url: "http://h"}, health_check: {path: /h, interval: "1ms"}}]`, "interval: must be at least 100ms"},
		{"invalid header name", `routes: [{path: "/x", upstream: {url: "http://h"}, request_transform: {headers: {add: {"X Bad Name": v}}}}]`, `invalid header name "X Bad Name"`},
		{"header value with newline", "routes: [{path: \"/x\", upstream: {url: \"http://h\"}, response_transform: {headers: {add: {X-A: \"a\\r\\nSet-Cookie: x\"}}}}]", "must not contain CR, LF or NUL"},
		{"invalid remove name", `routes: [{path: "/x", upstream: {url: "http://h"}, request_transform: {headers: {remove: ["Bad:Name"]}}}]`, `invalid header name "Bad:Name"`},
		{"invalid auth header", `routes: [{path: "/x", upstream: {url: "http://h"}, auth: {type: api_key, header: "X API", keys: [k]}}]`, `invalid header name "X API"`},
		{"$body in a header", `routes: [{path: "/x", upstream: {url: "http://h"}, request_transform: {headers: {add: {X-B: $body}}}}]`, "only valid inside a body envelope"},
		{"$body in a mapping", `routes: [{path: "/x", upstream: {url: "http://h"}, request_transform: {body: {mapping: {whole: $body}}}}]`, "only valid inside a body envelope"},
		{"conflict missed by neighbour check", `routes: [{path: "/x", upstream: {url: "http://h"}, request_transform: {body: {mapping: {user: a, user-x: b, user.id: c}}}}]`, `"user" and "user.id" conflict`},
		{"empty path segment", `routes: [{path: "/x", upstream: {url: "http://h"}, request_transform: {body: {mapping: {user..id: a}}}}]`, "destination must be a dot path"},
		{"unknown variable", `routes: [{path: "/x", upstream: {url: "http://h"}, request_transform: {headers: {add: {X-T: "$request_tme"}}}}]`, `unknown variable "$request_tme"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.yaml))
			if err == nil {
				t.Fatalf("expected an error containing %q", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not contain %q", err, tt.want)
			}
		})
	}
}

func TestValidationReportsAllErrors(t *testing.T) {
	_, err := Parse([]byte(`
gateway: {port: -1}
routes:
  - path: "nope"
    upstream: {}
`))
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"gateway.port", "routes[0].path", "routes[0].upstream"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %s:\n%v", want, err)
		}
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "missing.yaml"))
	if err == nil || !strings.Contains(err.Error(), "reading config") {
		t.Fatalf("want a read error, got %v", err)
	}
}

func TestLoadNamesFileInError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(path, []byte("routes: []"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("error should name the file, got %v", err)
	}
}
