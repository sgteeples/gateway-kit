package upstream

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/sgteeples/gateway-kit/internal/config"
	"github.com/sgteeples/gateway-kit/internal/httperr"
)

// Retry re-sends a request when the upstream answers with one of the
// configured status codes.
//
// Gateway-side failures are treated as the status the client would otherwise
// get (connection failure = 502, timeout = 504), so whether they're retried is
// decided by the same `on` list rather than by a hidden rule.
//
// Retries follow the config even for POST/PUT. A route that enables retry is
// trusting its upstream to be idempotent; see DECISIONS.md.
type Retry struct {
	attempts int // total, including the first
	backoff  string
	initial  time.Duration
	on       map[int]bool
	next     http.RoundTripper
	logger   *slog.Logger
	sleep    func(context.Context, time.Duration) error
}

func NewRetry(cfg config.Retry, next http.RoundTripper, logger *slog.Logger) *Retry {
	on := make(map[int]bool, len(cfg.On))
	for _, code := range cfg.On {
		on[code] = true
	}
	return &Retry{
		attempts: cfg.Attempts,
		backoff:  cfg.Backoff,
		initial:  cfg.InitialDelay.Std(),
		on:       on,
		next:     next,
		logger:   logger,
		sleep:    sleepCtx,
	}
}

func (r *Retry) RoundTrip(req *http.Request) (*http.Response, error) {
	// A streamed body can't be sent twice. Routes with retry buffer theirs,
	// which is what sets GetBody.
	replayable := req.Body == nil || req.Body == http.NoBody || req.GetBody != nil

	for attempt := 1; ; attempt++ {
		if attempt > 1 && req.GetBody != nil {
			body, err := req.GetBody()
			if err != nil {
				return nil, err
			}
			req = req.Clone(req.Context())
			req.Body = body
		}

		resp, err := r.next.RoundTrip(req)
		status, retryable := r.retryable(req, resp, err)
		if !retryable || attempt >= r.attempts || !replayable {
			// Out of attempts: the client gets the upstream's real last answer.
			return resp, err
		}

		delay := r.delay(attempt)
		r.logger.Info("retrying upstream request", "attempt", attempt+1, "of", r.attempts, "after_status", status, "delay", delay.String())
		discard(resp)
		if err := r.sleep(req.Context(), delay); err != nil {
			return nil, err
		}
	}
}

func (r *Retry) retryable(req *http.Request, resp *http.Response, err error) (status int, ok bool) {
	if err == nil {
		return resp.StatusCode, r.on[resp.StatusCode]
	}
	if req.Context().Err() != nil && errors.Is(err, context.Canceled) {
		return 0, false // the client has gone
	}
	status = httperr.From(err).Status
	return status, r.on[status]
}

// delay is the wait before attempt n+1: initial each time for fixed, and
// initial * 2^(n-1) for exponential (capped at 2^16 to avoid overflow).
func (r *Retry) delay(n int) time.Duration {
	if r.backoff != config.BackoffExponential {
		return r.initial
	}
	return r.initial << min(n-1, 16)
}

// discard drains (a bounded amount of) a response we're about to throw away,
// so its connection can be reused.
func discard(resp *http.Response) {
	if resp == nil {
		return
	}
	_, _ = io.CopyN(io.Discard, resp.Body, 64<<10)
	resp.Body.Close()
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
