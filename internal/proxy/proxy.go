// Package proxy is the bridge between the client-facing and upstream-facing
// halves of the pipeline. It turns the incoming request into an outbound one,
// hands it to the route's RoundTripper chain, and copies the response back.
// It is written by hand on net/http rather than httputil.ReverseProxy.
package proxy

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/textproto"
	"strings"

	"github.com/sgteeples/gateway-kit/internal/httperr"
	"github.com/sgteeples/gateway-kit/internal/netx"
)

// DefaultMaxBodyBytes caps request bodies the gateway has to buffer.
const DefaultMaxBodyBytes = 10 << 20

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
		h.Logger.Warn("upstream request failed", "path", r.URL.Path, "status", e.Status, "err", err)
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
		// Headers are already sent, so all we can do is log and cut the response short.
		h.Logger.Warn("copying upstream response body failed", "path", r.URL.Path, "err", err)
	}
}

func (h *Handler) outboundRequest(r *http.Request) (*http.Request, error) {
	path := r.URL.Path
	if h.StripPrefix != "" {
		path = stripPrefix(path, h.StripPrefix)
	}

	var body io.Reader = r.Body
	if h.BufferBody {
		buf, err := readBody(r.Body, h.maxBodyBytes())
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
	return DefaultMaxBodyBytes
}

func readBody(body io.Reader, limit int64) ([]byte, error) {
	buf, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, &httperr.Error{Status: http.StatusBadRequest, Code: "bad_request", Message: "could not read request body", Err: err}
	}
	if int64(len(buf)) > limit {
		return nil, &httperr.Error{Status: http.StatusRequestEntityTooLarge, Code: "payload_too_large", Err: errors.New("request body over limit")}
	}
	return buf, nil
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
