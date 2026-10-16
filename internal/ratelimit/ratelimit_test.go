package ratelimit

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sgteeples/gateway-kit/internal/config"
)

// epoch is aligned to every window used below.
var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) set(d time.Duration)     { c.t = epoch.Add(d) }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newLimiter(strategy string, limit int, window time.Duration) (*Limiter, *fakeClock) {
	clock := &fakeClock{t: epoch}
	return New(config.RateLimit{Requests: limit, Window: config.Duration(window), Strategy: strategy}, clock.now), clock
}

func allowN(l *Limiter, key string, n int) int {
	allowed := 0
	for range n {
		if ok, _ := l.Allow(key); ok {
			allowed++
		}
	}
	return allowed
}

func TestFixedWindow(t *testing.T) {
	l, clock := newLimiter(config.StrategyFixedWindow, 3, 10*time.Second)

	if got := allowN(l, "k", 5); got != 3 {
		t.Fatalf("allowed %d of 5, want 3", got)
	}
	clock.advance(4 * time.Second)
	ok, retry := l.Allow("k")
	if ok || retry != 6*time.Second {
		t.Errorf("Allow = %v, retry %v; want denied, retry 6s (until window end)", ok, retry)
	}
	clock.set(10 * time.Second)
	if ok, _ := l.Allow("k"); !ok {
		t.Error("new window should allow again")
	}
}

func TestBoundaryBurst(t *testing.T) {
	// 3 requests at the end of one window, then a burst right after the boundary.
	for _, tt := range []struct {
		strategy string
		want     int
	}{
		{config.StrategyFixedWindow, 3},   // fixed lets the whole burst through: 6 in ~1s
		{config.StrategySlidingWindow, 0}, // sliding still counts the previous window
	} {
		t.Run(tt.strategy, func(t *testing.T) {
			l, clock := newLimiter(tt.strategy, 3, 10*time.Second)
			clock.set(9 * time.Second)
			allowN(l, "k", 3)
			clock.set(10 * time.Second)
			if got := allowN(l, "k", 3); got != tt.want {
				t.Errorf("allowed %d right after the boundary, want %d", got, tt.want)
			}
		})
	}
}

func TestSlidingWindowDecays(t *testing.T) {
	l, clock := newLimiter(config.StrategySlidingWindow, 3, 10*time.Second)
	allowN(l, "k", 3)

	// Halfway through the next window, the previous 3 count as 1.5, leaving room for 1.
	clock.set(15 * time.Second)
	if got := allowN(l, "k", 3); got != 1 {
		t.Errorf("allowed %d halfway into the next window, want 1", got)
	}
}

func TestSlidingRetryAfterIsAccurate(t *testing.T) {
	l, clock := newLimiter(config.StrategySlidingWindow, 3, 10*time.Second)
	allowN(l, "k", 3)
	clock.set(10 * time.Second)

	ok, retry := l.Allow("k")
	if ok {
		t.Fatal("should be denied at the boundary")
	}
	// Needs 3*(1-e/10) <= 2, i.e. e >= 3.33s.
	if retry < 3300*time.Millisecond || retry > 3400*time.Millisecond {
		t.Errorf("retryAfter = %v, want ~3.33s", retry)
	}
	clock.advance(retry - 50*time.Millisecond)
	if ok, _ := l.Allow("k"); ok {
		t.Error("allowed before retryAfter elapsed")
	}
	clock.advance(50 * time.Millisecond)
	if ok, _ := l.Allow("k"); !ok {
		t.Error("still denied after waiting retryAfter")
	}
}

func TestIdleGapResetsHistory(t *testing.T) {
	l, clock := newLimiter(config.StrategySlidingWindow, 3, 10*time.Second)
	allowN(l, "k", 3)
	clock.set(25 * time.Second) // two windows later
	if got := allowN(l, "k", 3); got != 3 {
		t.Errorf("allowed %d after a long gap, want 3", got)
	}
}

func TestKeysAreIsolated(t *testing.T) {
	l, _ := newLimiter(config.StrategyFixedWindow, 2, time.Minute)
	allowN(l, "1.1.1.1", 5)
	if got := allowN(l, "2.2.2.2", 2); got != 2 {
		t.Errorf("second key allowed %d, want 2", got)
	}
}

// TestConcurrentRequests is the "50 requests hit a rate-limited route at once"
// case: exactly `limit` must get through, never more. Run with -race.
func TestConcurrentRequests(t *testing.T) {
	for _, strategy := range []string{config.StrategyFixedWindow, config.StrategySlidingWindow} {
		t.Run(strategy, func(t *testing.T) {
			l := New(config.RateLimit{Requests: 10, Window: config.Duration(time.Minute), Strategy: strategy}, time.Now)

			var allowed atomic.Int64
			var wg sync.WaitGroup
			start := make(chan struct{})
			for range 50 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					if ok, _ := l.Allow("same-client"); ok {
						allowed.Add(1)
					}
				}()
			}
			close(start)
			wg.Wait()

			// Can only fail if the test straddles a minute boundary, which
			// the 50 calls are far too fast to do in practice.
			if got := allowed.Load(); got != 10 {
				t.Errorf("allowed %d of 50 concurrent requests, want exactly 10", got)
			}
		})
	}
}

func TestSweepDropsIdleBuckets(t *testing.T) {
	l, clock := newLimiter(config.StrategyFixedWindow, 1, 10*time.Second)
	l.Allow("old")
	clock.set(30 * time.Second)
	l.Allow("fresh")
	l.Sweep()

	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.buckets["old"]; ok {
		t.Error("idle bucket should be swept")
	}
	if _, ok := l.buckets["fresh"]; !ok {
		t.Error("active bucket should be kept")
	}
}
