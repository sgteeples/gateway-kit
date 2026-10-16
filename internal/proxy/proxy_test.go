package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStripPrefix(t *testing.T) {
	tests := []struct{ path, prefix, want string }{
		{"/api/products/123", "/api/products", "/123"},
		{"/api/products", "/api/products", "/"},
		{"/api/legacy/v1/data", "/api/legacy", "/v1/data"},
		{"/anything", "/", "/anything"},
	}
	for _, tt := range tests {
		if got := stripPrefix(tt.path, tt.prefix); got != tt.want {
			t.Errorf("stripPrefix(%q, %q) = %q, want %q", tt.path, tt.prefix, got, tt.want)
		}
	}
}

func TestRemoveHopByHop(t *testing.T) {
	h := http.Header{}
	h.Set("Connection", "keep-alive, X-Session-Hop")
	h.Set("X-Session-Hop", "1")
	h.Set("Keep-Alive", "timeout=5")
	h.Set("Transfer-Encoding", "chunked")
	h.Set("X-End-To-End", "keep me")

	removeHopByHop(h)

	for _, gone := range []string{"Connection", "X-Session-Hop", "Keep-Alive", "Transfer-Encoding"} {
		if h.Get(gone) != "" {
			t.Errorf("%s should have been removed", gone)
		}
	}
	if h.Get("X-End-To-End") != "keep me" {
		t.Error("end-to-end header was removed")
	}
}

func TestForwardedHeaders(t *testing.T) {
	r := httptest.NewRequest("GET", "http://gateway.example/x", nil)
	r.RemoteAddr = "203.0.113.7:51234"
	r.Header.Set("X-Forwarded-For", "198.51.100.1")

	h := http.Header{}
	h.Set("X-Forwarded-For", r.Header.Get("X-Forwarded-For"))
	addForwardedHeaders(h, r)

	if got := h.Get("X-Forwarded-For"); got != "198.51.100.1, 203.0.113.7" {
		t.Errorf("X-Forwarded-For = %q", got)
	}
	if got := h.Get("X-Forwarded-Host"); got != "gateway.example" {
		t.Errorf("X-Forwarded-Host = %q", got)
	}
	if got := h.Get("X-Forwarded-Proto"); got != "http" {
		t.Errorf("X-Forwarded-Proto = %q", got)
	}
}
