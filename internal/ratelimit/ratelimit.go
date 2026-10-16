// Package ratelimit implements fixed-window and sliding-window request limits
// keyed by an arbitrary string (a client IP, or one shared key).
package ratelimit

import (
	"context"
	"math"
	"sync"
	"time"

	"github.com/sgteeples/gateway-kit/internal/config"
)

// Limiter counts requests per key in windows aligned to multiples of the window
// length.
//
// Fixed window allows `limit` requests per aligned window. It is cheap and exact,
// but a client can send `limit` at the end of one window and `limit` again at
// the start of the next.
//
// Sliding window uses the sliding-window-counter approximation: the previous
// window's count is weighted by how much of it still overlaps the trailing
// window. That closes the boundary burst with O(1) memory per key, where an
// exact timestamp log would need O(limit) per key.
//
// The check and the increment happen under one mutex, so concurrent requests
// can never both see room for the last slot.
type Limiter struct {
	limit   int
	window  time.Duration
	sliding bool
	now     func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	start      time.Time // start of the current aligned window
	curr, prev int       // counts for the current and previous windows
}

// New builds a limiter from a normalized rate limit config. now is injectable
// for tests; pass time.Now in production.
func New(cfg config.RateLimit, now func() time.Time) *Limiter {
	return &Limiter{
		limit:   cfg.Requests,
		window:  cfg.Window.Std(),
		sliding: cfg.Strategy == config.StrategySlidingWindow,
		now:     now,
		buckets: make(map[string]*bucket),
	}
}

// Allow records a request for key if it fits. If it doesn't, it returns how
// long the caller should wait before trying again.
func (l *Limiter) Allow(key string) (allowed bool, retryAfter time.Duration) {
	now := l.now()
	start := now.Truncate(l.window)
	elapsed := now.Sub(start)

	l.mu.Lock()
	defer l.mu.Unlock()

	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{start: start}
		l.buckets[key] = b
	}
	b.advance(start, l.window)

	if !l.sliding {
		if b.curr >= l.limit {
			return false, l.window - elapsed
		}
		b.curr++
		return true, 0
	}

	prevWeight := 1 - float64(elapsed)/float64(l.window)
	if float64(b.prev)*prevWeight+float64(b.curr)+1 > float64(l.limit) {
		return false, l.slidingRetryAfter(b, elapsed)
	}
	b.curr++
	return true, 0
}

// advance rolls the bucket forward to the window starting at start.
func (b *bucket) advance(start time.Time, window time.Duration) {
	switch gap := start.Sub(b.start); {
	case gap <= 0:
		return
	case gap == window:
		b.prev, b.curr = b.curr, 0
	default: // more than one window went by with no requests
		b.prev, b.curr = 0, 0
	}
	b.start = start
}

// slidingRetryAfter solves for when the weighted estimate first leaves room
// for one more request.
func (l *Limiter) slidingRetryAfter(b *bucket, elapsed time.Duration) time.Duration {
	w := float64(l.window)
	room := float64(l.limit - 1) // the estimate must drop to this to admit one more
	var wait float64
	if float64(b.curr) > room {
		// This window alone is full. Wait for it to end, then for its count
		// (as next window's "previous") to decay enough.
		wait = (w - float64(elapsed)) + w*(1-room/float64(b.curr))
	} else {
		// The previous window's tail is what's over the limit.
		wait = w*(1-(room-float64(b.curr))/float64(b.prev)) - float64(elapsed)
	}
	// Round up: a client that waits exactly this long must be let in, which
	// truncating the float (and its rounding error) would not guarantee.
	ms := math.Ceil(wait / float64(time.Millisecond))
	return max(time.Duration(ms)*time.Millisecond, time.Millisecond)
}

// Sweep drops buckets idle long enough that they no longer affect any
// decision, so one-off client IPs don't accumulate forever.
func (l *Limiter) Sweep() {
	cutoff := l.now().Add(-2 * l.window)
	l.mu.Lock()
	defer l.mu.Unlock()
	for key, b := range l.buckets {
		if b.start.Before(cutoff) {
			delete(l.buckets, key)
		}
	}
}

// RunJanitor calls Sweep periodically until ctx is cancelled.
func (l *Limiter) RunJanitor(ctx context.Context) {
	ticker := time.NewTicker(max(l.window, time.Second))
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			l.Sweep()
		}
	}
}
