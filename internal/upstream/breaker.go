package upstream

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/sgteeples/gateway-kit/internal/config"
	"github.com/sgteeples/gateway-kit/internal/httperr"
)

type breakerState int

const (
	closed breakerState = iota
	open
	halfOpen
)

func (s breakerState) String() string {
	return [...]string{"closed", "open", "half-open"}[s]
}

// Breaker stops calling an upstream that keeps failing.
//
//	closed:    calls pass through; trips to open after `threshold` failures
//	           within a rolling `window` (not "N in a row": successes don't
//	           reset the count, old failures just age out)
//	open:      calls fail fast with 503 and retry_after, without reaching the
//	           upstream, until `cooldown` elapses
//	half-open: exactly one probe call is let through; success closes the
//	           breaker, failure re-opens it for a full cooldown
//
// A failure is a transport error, a timeout, or a 5xx response. Requests the
// client abandoned don't count.
type Breaker struct {
	threshold int
	window    time.Duration
	cooldown  time.Duration
	next      http.RoundTripper
	logger    *slog.Logger
	now       func() time.Time

	mu        sync.Mutex
	state     breakerState
	failures  []time.Time // the most recent failures, oldest first, at most threshold
	openUntil time.Time
	probing   bool // a half-open probe is in flight
}

func NewBreaker(cfg config.CircuitBreaker, next http.RoundTripper, logger *slog.Logger) *Breaker {
	return &Breaker{
		threshold: cfg.Threshold,
		window:    cfg.Window.Std(),
		cooldown:  cfg.Cooldown.Std(),
		next:      next,
		logger:    logger,
		now:       time.Now,
		failures:  make([]time.Time, 0, cfg.Threshold),
	}
}

func (b *Breaker) RoundTrip(req *http.Request) (*http.Response, error) {
	probe, err := b.admit()
	if err != nil {
		return nil, err
	}
	resp, err := b.next.RoundTrip(req)
	if req.Context().Err() != nil && errors.Is(err, context.Canceled) {
		// The client hung up; that says nothing about the upstream.
		b.release(probe)
		return resp, err
	}
	b.record(probe, err != nil || resp.StatusCode >= 500)
	return resp, err
}

// admit decides whether a call may go to the upstream, and whether it is the
// half-open probe.
func (b *Breaker) admit() (probe bool, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case open:
		now := b.now()
		if now.Before(b.openUntil) {
			return false, unavailable(b.openUntil.Sub(now))
		}
		b.transition(halfOpen)
		fallthrough
	case halfOpen:
		if b.probing {
			// Someone else is already testing the upstream; it's about to
			// either recover or re-open, so suggest a short wait.
			return false, unavailable(time.Second)
		}
		b.probing = true
		return true, nil
	default:
		return false, nil
	}
}

func (b *Breaker) record(probe, failed bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()

	if probe {
		b.probing = false
		if failed {
			b.trip(now)
		} else {
			b.failures = b.failures[:0]
			b.transition(closed)
		}
		return
	}
	// Results of calls admitted before the breaker tripped are ignored; the
	// probe alone decides recovery.
	if b.state != closed || !failed {
		return
	}
	if len(b.failures) == b.threshold {
		copy(b.failures, b.failures[1:])
		b.failures = b.failures[:b.threshold-1]
	}
	b.failures = append(b.failures, now)
	if len(b.failures) == b.threshold && now.Sub(b.failures[0]) <= b.window {
		b.trip(now)
	}
}

func (b *Breaker) release(probe bool) {
	if !probe {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.probing = false
}

func (b *Breaker) trip(now time.Time) {
	b.openUntil = now.Add(b.cooldown)
	b.failures = b.failures[:0]
	b.transition(open)
}

func (b *Breaker) transition(to breakerState) {
	if b.state == to {
		return
	}
	b.logger.Warn("circuit breaker state change", "from", b.state.String(), "to", to.String())
	b.state = to
}

func unavailable(retryAfter time.Duration) error {
	// No Message: the spec gives this body exactly, as error + retry_after.
	return &httperr.Error{Status: http.StatusServiceUnavailable, Code: "service_unavailable", RetryAfter: retryAfter}
}
