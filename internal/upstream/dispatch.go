package upstream

import (
	"context"
	"io"
	"net/http"
	"strings"
	"time"
)

// Dispatch is the innermost policy: it picks a target, points the request at
// it, and bounds the attempt with the route timeout. It sits below retry, so
// every retry attempt can land on a different target and gets its own timeout.
type Dispatch struct {
	Balancer Balancer
	Timeout  time.Duration
	Next     http.RoundTripper
}

func (d *Dispatch) RoundTrip(req *http.Request) (*http.Response, error) {
	target, err := d.Balancer.Next()
	if err != nil {
		return nil, err
	}

	// The attempt is bound to the client's context too, so a client that hangs
	// up cancels the upstream call instead of leaving it running.
	ctx, cancel := context.WithTimeout(req.Context(), d.Timeout)
	out := req.Clone(ctx)
	out.URL.Scheme = target.URL.Scheme
	out.URL.Host = target.URL.Host
	out.URL.Path = joinPath(target.URL.Path, req.URL.Path)
	out.URL.RawPath = ""
	out.Host = target.URL.Host

	resp, err := d.Next.RoundTrip(out)
	if err != nil {
		cancel()
		return nil, err
	}
	// The timeout covers reading the body as well, so the context can only be
	// released once the caller has finished with it.
	resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

func joinPath(base, path string) string {
	base = strings.TrimSuffix(base, "/")
	if base == "" {
		return path
	}
	return base + path
}

type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}
