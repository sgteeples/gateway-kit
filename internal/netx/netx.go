// Package netx holds small request helpers shared across gateway layers.
package netx

import (
	"net"
	"net/http"
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
