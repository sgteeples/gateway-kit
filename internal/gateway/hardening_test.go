package gateway

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// rawRequest writes raw bytes to the gateway and returns its parsed response,
// for requests the Go client won't send (slow bodies, unusual paths).
func rawRequest(t *testing.T, h *harness, raw string, afterWrite func(net.Conn)) *http.Response {
	t.Helper()
	conn, err := net.Dial("tcp", strings.TrimPrefix(h.gateway.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if _, err := io.WriteString(conn, raw); err != nil {
		t.Fatal(err)
	}
	if afterWrite != nil {
		afterWrite(conn)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("reading response: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestTruncatedUpstreamBodyAbortsClientConnection(t *testing.T) {
	// The upstream sends part of the body, then stalls past the route timeout.
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("PART1-"))
		w.(http.Flusher).Flush()
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
		w.Write([]byte("PART2-END"))
	}))
	t.Cleanup(up.Close)
	h := newHarness(t, `routes: [{path: /, upstream: {url: %q, timeout: "200ms"}}]`, up.URL)

	// The client must see an error, never a clean (truncated) success. When it
	// surfaces depends on buffering: before the headers arrive, or mid-body.
	resp, err := http.Get(h.gateway.URL + "/stream")
	if err == nil {
		defer resp.Body.Close()
		body, readErr := io.ReadAll(resp.Body)
		if readErr == nil {
			t.Fatalf("client read %q with status %d as a complete response; truncation must surface as an error", body, resp.StatusCode)
		}
	}
}

func TestSlowRequestBodyTimesOut(t *testing.T) {
	_, up := newUpstream(t, "a")
	for _, tt := range []struct{ name, route string }{
		{"streamed body", `{path: /, upstream: {url: %q, timeout: "300ms"}}`},
		{"buffered body (retry route)", `{path: /, upstream: {url: %q, timeout: "300ms"}, retry: {attempts: 2, on: [503]}}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, "routes: ["+tt.route+"]", up)
			start := time.Now()
			// Promise 100 bytes, send 1, then go quiet.
			resp := rawRequest(t, h, "POST /x HTTP/1.1\r\nHost: gw\r\nContent-Length: 100\r\n\r\n{", nil)
			if resp.StatusCode != http.StatusRequestTimeout {
				t.Errorf("status = %d, want 408", resp.StatusCode)
			}
			if elapsed := time.Since(start); elapsed > 2*time.Second {
				t.Errorf("took %v; a slow body must be cut off at the route timeout", elapsed)
			}
		})
	}
}

func TestBodyDeadlineDoesNotCutRetries(t *testing.T) {
	// Retries take longer than one route timeout in total; the body deadline
	// must be lifted once the body is read, or it would cancel them.
	up, upURL := newUpstream(t, "a")
	h := newHarness(t, `
routes:
  - path: /
    upstream: {url: %q, timeout: "200ms"}
    retry: {attempts: 3, initial_delay: "150ms", on: [503]}
`, upURL)

	up.FailNext(2, http.StatusServiceUnavailable)
	resp := h.do("POST", "/", strings.NewReader(`{"a":1}`), http.Header{"Content-Type": {"application/json"}})
	wantStatus(t, resp, http.StatusOK)
	if up.Requests() != 3 {
		t.Errorf("upstream saw %d attempts, want 3", up.Requests())
	}
}

func TestAmbiguousPathsRejected(t *testing.T) {
	_, up := newUpstream(t, "a")
	h := newHarness(t, `
routes:
  - {path: /, upstream: {url: %q}}
  - {path: /api/internal, upstream: {url: %[1]q}, auth: {type: api_key, header: X-API-Key, keys: [k]}}
`, up)

	for _, path := range []string{
		"/api/internal;x",       // Tomcat/Spring would serve /api/internal, bypassing auth
		"/api/internal%3Bx",     // the same, encoded
		"/api%2Finternal",       // encoded slash: one segment here, two upstream
		"/api%5Cinternal",       // encoded backslash
		"/api%252Finternal",     // double-encoded slash
		"/api/internal%00.json", // control character
	} {
		t.Run(path, func(t *testing.T) {
			resp := rawRequest(t, h, "GET "+path+" HTTP/1.1\r\nHost: gw\r\n\r\n", nil)
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", resp.StatusCode)
			}
		})
	}

	// Ordinary encoding still works.
	resp := rawRequest(t, h, "GET /files/my%20report.pdf HTTP/1.1\r\nHost: gw\r\n\r\n", nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("%%20 in a path: status = %d, want 200", resp.StatusCode)
	}
}
