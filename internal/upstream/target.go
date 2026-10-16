// Package upstream holds the upstream-facing half of the pipeline: picking a
// target and the resilience policies around each call. Every policy is an
// http.RoundTripper that wraps the next one, so they compose and can be tested
// with a fake transport, and none of them writes to the client directly.
package upstream

import (
	"fmt"
	"net/url"
	"sync/atomic"
)

// Target is one upstream server. Health is updated by the health checker and
// read by balancers on every request, so it is atomic.
type Target struct {
	URL       *url.URL
	Weight    int
	unhealthy atomic.Bool
}

func NewTarget(rawURL string, weight int) (*Target, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parsing target url %q: %w", rawURL, err)
	}
	return &Target{URL: u, Weight: weight}, nil
}

func (t *Target) Healthy() bool      { return !t.unhealthy.Load() }
func (t *Target) SetHealthy(ok bool) { t.unhealthy.Store(!ok) }
func (t *Target) String() string     { return t.URL.String() }
