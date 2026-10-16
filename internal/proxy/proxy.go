// Package proxy is the bridge between the client-facing and upstream-facing
// halves of the pipeline. It turns the incoming request into an outbound one,
// hands it to the route's RoundTripper chain, and copies the response back.
// It is written by hand on net/http rather than httputil.ReverseProxy.
package proxy

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/textproto"
	"strings"

	"github.com/sgteeples/gateway-kit/internal/httperr"
	"github.com/sgteeples/gateway-kit/internal/netx"
)

// Handler forwards requests for one route.
type Handler struct {
	// Transport is the route's upstream chain. It receives a request whose URL
	// holds only the path and query; the Dispatch layer fills in the target.
	Transport http.RoundTripper
	// StripPrefix is removed from the path before forwarding ("" = keep it).
	StripPrefix string
	// BufferBody reads the whole request body up front so it can be sent more
	// than once (retries) or rewritten (body transforms). Otherwise it streams.
	BufferBody   bool
	MaxBodyBytes int64
	Logger       *slog.Logger
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	out, err := h.outboundRequest(r)
	if err != nil {
		httperr.Write(w, httperr.From(err))
		return
	}

	resp, err := h.Transport.RoundTrip(out)
	if err != nil {
		if r.Context().Err() != nil {
			// The client has gone away; nobody is left to answer.
			return
		}
		e := httperr.From(err)
		if e.Err != nil {
			// A real upstream failure. Policy rejections (e.g. an open
			// circuit breaker) never reached the upstream and are logged
			// by the access log alone.
			h.Logger.Warn("upstream request failed", "path", r.URL.Path, "status", e.Status, "err", err)
		}
		httperr.Write(w, e)
		return
	}
	defer resp.Body.Close()

	header := resp.Header.Clone()
	removeHopByHop(header)
	for k, vv := range header {
		w.Header()[k] = vv
	}
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil && r.Context().Err() == nil {
		h.Logger.Warn("upstream response body failed mid-stream; aborting the client connection", "path", r.URL.Path, "err", err)
		// The status line is already sent, so the only way left to signal the
		// failure is to drop the connection. Returning normally would let the
		// server finish a chunked response cleanly, and a truncated body would
		// look complete. (httputil.ReverseProxy does the same.)
		panic(http.ErrAbortHandler)
	}
}

func (h *Handler) outboundRequest(r *http.Request) (*http.Request, error) {
	path := r.URL.Path
	if h.StripPrefix != "" {
		path = stripPrefix(path, h.StripPrefix)
	}

	var body io.Reader = r.Body
	if h.BufferBody {
		buf, err := netx.ReadLimited(r.Body, h.maxBodyBytes())
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(buf) // also gives the request a GetBody for replays
	}

	out, err := http.NewRequestWithContext(r.Context(), r.Method, "", body)
	if err != nil {
		return nil, err
	}
	out.URL.Path = path
	out.URL.RawQuery = r.URL.RawQuery
	if !h.BufferBody {
		out.ContentLength = r.ContentLength
	}

	out.Header = r.Header.Clone()
	removeHopByHop(out.Header)
	if _, ok := out.Header["User-Agent"]; !ok {
		out.Header.Set("User-Agent", "") // stop net/http adding its own
	}
	addForwardedHeaders(out.Header, r)
	return out, nil
}

func (h *Handler) maxBodyBytes() int64 {
	if h.MaxBodyBytes > 0 {
		return h.MaxBodyBytes
	}
	return netx.MaxBodyBytes
}

// stripPrefix removes a route prefix: /api/products/123 -> /123, and the bare
// prefix becomes "/".
func stripPrefix(path, prefix string) string {
	if prefix == "/" {
		return path
	}
	rest := strings.TrimPrefix(path, prefix)
	if rest == "" {
		return "/"
	}
	return rest
}

func addForwardedHeaders(h http.Header, r *http.Request) {
	ip := netx.ClientIP(r)
	if prior := h.Values("X-Forwarded-For"); len(prior) > 0 {
		ip = strings.Join(prior, ", ") + ", " + ip
	}
	h.Set("X-Forwarded-For", ip)
	h.Set("X-Forwarded-Host", r.Host)
	proto := "http"
	if r.TLS != nil {
		proto = "https"
	}
	h.Set("X-Forwarded-Proto", proto)
}

// Hop-by-hop headers describe a single connection and must not be forwarded
// (RFC 9110 §7.6.1).
var hopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

func removeHopByHop(h http.Header) {
	for _, v := range h.Values("Connection") {
		for _, name := range strings.Split(v, ",") {
			if name = textproto.TrimString(name); name != "" {
				h.Del(name)
			}
		}
	}
	for _, name := range hopHeaders {
		h.Del(name)
	}
}
