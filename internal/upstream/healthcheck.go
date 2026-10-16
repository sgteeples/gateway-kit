package upstream

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/sgteeples/gateway-kit/internal/config"
)

// HealthChecker actively probes a route's targets and flips their healthy flag,
// which the balancers read on every request.
//
// A target is marked unhealthy after `unhealthy_threshold` consecutive failed
// probes, and healthy again after one success. Targets start healthy, so the
// gateway serves traffic during boot instead of waiting for a first verdict.
// Probes go straight to the transport, bypassing auth, transforms, retry and
// the breaker, which are all policies for client traffic.
type HealthChecker struct {
	targets   []*Target
	path      string
	interval  time.Duration
	timeout   time.Duration
	threshold int
	transport http.RoundTripper
	logger    *slog.Logger

	failures []int // consecutive failures per target; each index is owned by one probe goroutine
	allDown  bool
}

// NewHealthChecker builds a checker. timeout bounds each probe; it is capped
// at the interval so probes can never pile up.
func NewHealthChecker(cfg config.HealthCheck, targets []*Target, transport http.RoundTripper, timeout time.Duration, logger *slog.Logger) *HealthChecker {
	return &HealthChecker{
		targets:   targets,
		path:      cfg.Path,
		interval:  cfg.Interval.Std(),
		timeout:   min(timeout, cfg.Interval.Std()),
		threshold: cfg.UnhealthyThreshold,
		transport: transport,
		logger:    logger,
		failures:  make([]int, len(targets)),
	}
}

// Run probes immediately, then every interval, until ctx is cancelled.
func (h *HealthChecker) Run(ctx context.Context) {
	ticker := time.NewTicker(h.interval)
	defer ticker.Stop()
	for {
		h.checkAll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// checkAll probes every target concurrently, so one slow target can't delay
// the verdict on the others.
func (h *HealthChecker) checkAll(ctx context.Context) {
	var wg sync.WaitGroup
	for i := range h.targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.check(ctx, i)
		}()
	}
	wg.Wait()

	allDown := true
	for _, t := range h.targets {
		allDown = allDown && !t.Healthy()
	}
	if allDown && !h.allDown {
		h.logger.Warn("all upstream targets are unhealthy; failing open and sending traffic to all of them")
	}
	h.allDown = allDown
}

func (h *HealthChecker) check(ctx context.Context, i int) {
	t := h.targets[i]
	if h.probe(ctx, t) {
		h.failures[i] = 0
		if !t.Healthy() {
			t.SetHealthy(true)
			h.logger.Info("upstream target is healthy again", "target", t.String())
		}
		return
	}
	if ctx.Err() != nil {
		return // shutting down; not the target's fault
	}
	h.failures[i]++
	if h.failures[i] >= h.threshold && t.Healthy() {
		t.SetHealthy(false)
		h.logger.Warn("upstream target marked unhealthy", "target", t.String(), "consecutive_failures", h.failures[i])
	}
}

func (h *HealthChecker) probe(ctx context.Context, t *Target) bool {
	ctx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()
	u := *t.URL
	u.Path = joinPath(u.Path, h.path)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return false
	}
	resp, err := h.transport.RoundTrip(req)
	if err != nil {
		return false
	}
	discard(resp)
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}
