package upstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sgteeples/gateway-kit/internal/config"
	"github.com/sgteeples/gateway-kit/internal/httperr"
)

func newBreaker(next http.RoundTripper) (*Breaker, *fakeClock) {
	clock := newFakeClock()
	b := NewBreaker(config.CircuitBreaker{
		Threshold: 3,
		Window:    config.Duration(10 * time.Second),
		Cooldown:  config.Duration(5 * time.Second),
	}, next, discardLogger)
	b.now = clock.now
	return b, clock
}

func call(b *Breaker) (*http.Response, error) {
	req, _ := http.NewRequest("GET", "http://upstream/", nil)
	return b.RoundTrip(req)
}

// wantOpen asserts the call was rejected by the breaker with the given
// retry_after. It uses Errorf so it is safe to call from other goroutines.
func wantOpen(t *testing.T, b *Breaker, retryAfter time.Duration) {
	t.Helper()
	_, err := call(b)
	var e *httperr.Error
	if !errors.As(err, &e) || e.Status != http.StatusServiceUnavailable || e.Code != "service_unavailable" {
		t.Errorf("want a 503 service_unavailable from the breaker, got %v", err)
		return
	}
	if e.RetryAfter != retryAfter {
		t.Errorf("retry_after = %v, want %v", e.RetryAfter, retryAfter)
	}
}

func wantPassed(t *testing.T, b *Breaker) {
	t.Helper()
	if _, err := call(b); err != nil {
		t.Fatalf("call should have reached the upstream, got %v", err)
	}
}

func TestBreakerTripsAtThreshold(t *testing.T) {
	up := script(result{status: 500})
	b, clock := newBreaker(up)

	for range 3 {
		wantPassed(t, b)
	}
	wantOpen(t, b, 5*time.Second)
	if up.Calls() != 3 {
		t.Errorf("upstream called %d times, want 3: an open breaker must not call it", up.Calls())
	}

	clock.advance(2 * time.Second)
	wantOpen(t, b, 3*time.Second) // retry_after counts down
}

func TestBreakerUsesRollingWindow(t *testing.T) {
	b, clock := newBreaker(script(result{status: 503}))

	wantPassed(t, b) // t=0
	clock.advance(6 * time.Second)
	wantPassed(t, b) // t=6
	clock.advance(6 * time.Second)
	wantPassed(t, b) // t=12: 3 failures, but spread over 12s > 10s window
	wantPassed(t, b) // t=12: failures at 6, 12, 12 are within the window -> trip
	wantOpen(t, b, 5*time.Second)
}

func TestBreakerSuccessesDoNotReset(t *testing.T) {
	up := script(result{status: 500}, result{status: 200}, result{status: 500}, result{status: 200}, result{status: 500})
	b, _ := newBreaker(up)
	for range 5 {
		wantPassed(t, b)
	}
	wantOpen(t, b, 5*time.Second)
}

func TestBreakerIgnoresClientErrors(t *testing.T) {
	b, _ := newBreaker(script(result{status: 404}))
	for range 10 {
		wantPassed(t, b)
	}
}

func TestBreakerCountsTransportErrors(t *testing.T) {
	b, _ := newBreaker(script(result{err: errors.New("connection refused")}))
	for range 3 {
		if _, err := call(b); err == nil {
			t.Fatal("transport error should be returned")
		}
	}
	wantOpen(t, b, 5*time.Second)
}

func TestBreakerIgnoresClientCancellation(t *testing.T) {
	b, _ := newBreaker(script(result{err: context.Canceled}))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for range 5 {
		req, _ := http.NewRequestWithContext(ctx, "GET", "http://upstream/", nil)
		b.RoundTrip(req)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state != closed {
		t.Errorf("state = %v; abandoned requests must not trip the breaker", b.state)
	}
}

func TestBreakerProbeSuccessCloses(t *testing.T) {
	up := script(result{status: 500}, result{status: 500}, result{status: 500}, result{status: 200})
	b, clock := newBreaker(up)
	for range 3 {
		wantPassed(t, b)
	}
	clock.advance(5 * time.Second)

	wantPassed(t, b) // the probe succeeds
	for range 5 {
		wantPassed(t, b) // closed again
	}
}

func TestBreakerProbeFailureReopens(t *testing.T) {
	b, clock := newBreaker(script(result{status: 500}))
	for range 3 {
		wantPassed(t, b)
	}
	clock.advance(5 * time.Second)

	wantPassed(t, b)              // the probe reaches the upstream and fails
	wantOpen(t, b, 5*time.Second) // a full new cooldown
}

// gate blocks every call until released, so a probe can be held in flight.
type gate struct {
	entered chan struct{}
	release chan struct{}
}

func (g *gate) RoundTrip(req *http.Request) (*http.Response, error) {
	g.entered <- struct{}{}
	<-g.release
	return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
}

func TestBreakerAllowsOnlyOneProbe(t *testing.T) {
	failing := script(result{status: 500})
	b, clock := newBreaker(failing)
	for range 3 {
		wantPassed(t, b)
	}
	clock.advance(5 * time.Second)

	g := &gate{entered: make(chan struct{}), release: make(chan struct{})}
	b.next = g

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		call(b) // the probe
	}()
	<-g.entered

	// While the probe is in flight, concurrent requests are turned away.
	var rejected sync.WaitGroup
	for range 20 {
		rejected.Add(1)
		go func() {
			defer rejected.Done()
			wantOpen(t, b, time.Second)
		}()
	}
	rejected.Wait()

	close(g.release)
	wg.Wait()
	b.next = script(result{status: 200})
	wantPassed(t, b) // probe succeeded, so closed
}
