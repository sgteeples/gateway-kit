// Package netx holds small request helpers shared across gateway layers.
package netx

import (
	"fmt"
	"io"
	"net"
	"net/http"

	"github.com/sgteeples/gateway-kit/internal/httperr"
)

// ClientIP returns the IP of the peer connected to the gateway.
//
// X-Forwarded-For is deliberately ignored: any client can set it, so trusting
// it would let callers pick their own rate-limit bucket. Supporting it safely
// needs a configured list of trusted proxies.
func ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// MaxBodyBytes caps any body the gateway has to hold in memory (for retries
// or body transforms). Streamed bodies are not limited.
const MaxBodyBytes = 10 << 20

// ReadLimited reads all of r, failing with 413 if it exceeds limit bytes.
func ReadLimited(r io.Reader, limit int64) ([]byte, error) {
	buf, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, &httperr.Error{Status: http.StatusBadRequest, Code: "bad_request", Message: "could not read request body", Err: err}
	}
	if int64(len(buf)) > limit {
		return nil, &httperr.Error{Status: http.StatusRequestEntityTooLarge, Code: "payload_too_large", Message: fmt.Sprintf("body exceeds %d bytes", limit)}
	}
	return buf, nil
}
