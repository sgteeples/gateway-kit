package upstream

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/sgteeples/gateway-kit/internal/config"
)

// newRetry returns a Retry whose sleeps are recorded instead of waited.
func newRetry(next http.RoundTripper, backoff string, on ...int) (*Retry, *[]time.Duration) {
	r := NewRetry(config.Retry{Attempts: 3, Backoff: backoff, InitialDelay: config.Duration(time.Second), On: on}, next, discardLogger)
	var slept []time.Duration
	r.sleep = func(ctx context.Context, d time.Duration) error {
		slept = append(slept, d)
		return ctx.Err()
	}
	return r, &slept
}

func get(t *testing.T, r http.RoundTripper) (*http.Response, error) {
	t.Helper()
	req, _ := http.NewRequest("GET", "http://upstream/", nil)
	return r.RoundTrip(req)
}

func TestRetryRecovers(t *testing.T) {
	up := script(result{status: 503}, result{status: 503}, result{status: 200})
	r, slept := newRetry(up, config.BackoffExponential, 502, 503, 504)

	resp, err := get(t, r)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("got %v %v, want 200", resp, err)
	}
	if up.Calls() != 3 {
		t.Errorf("calls = %d, want 3", up.Calls())
	}
	if want := []time.Duration{time.Second, 2 * time.Second}; !slices.Equal(*slept, want) {
		t.Errorf("exponential delays = %v, want %v", *slept, want)
	}
}

func TestRetryFixedBackoff(t *testing.T) {
	r, slept := newRetry(script(result{status: 503}), config.BackoffFixed, 503)
	get(t, r)
	if want := []time.Duration{time.Second, time.Second}; !slices.Equal(*slept, want) {
		t.Errorf("fixed delays = %v, want %v", *slept, want)
	}
}

func TestRetryGivesUpWithLastResponse(t *testing.T) {
	up := script(result{status: 502}, result{status: 503}, result{status: 504})
	r, _ := newRetry(up, config.BackoffFixed, 502, 503, 504)

	resp, err := get(t, r)
	if err != nil || resp.StatusCode != 504 {
		t.Fatalf("got %v %v, want the last real response (504)", resp, err)
	}
	if up.Calls() != 3 {
		t.Errorf("calls = %d, want 3 (attempts is the total)", up.Calls())
	}
}

func TestRetryOnlyListedStatuses(t *testing.T) {
	up := script(result{status: 500})
	r, _ := newRetry(up, config.BackoffFixed, 502, 503, 504)
	resp, _ := get(t, r)
	if resp.StatusCode != 500 || up.Calls() != 1 {
		t.Errorf("500 is not in `on`: got status %d after %d calls", resp.StatusCode, up.Calls())
	}
}

func TestRetryMapsTransportErrors(t *testing.T) {
	refused := errors.New("dial tcp: connection refused")
	timeout := fmt.Errorf("attempt: %w", context.DeadlineExceeded)

	tests := []struct {
		name      string
		err       error
		on        []int
		wantCalls int
	}{
		{"connection error retried as 502", refused, []int{502}, 3},
		{"connection error not retried without 502", refused, []int{503}, 1},
		{"timeout retried as 504", timeout, []int{504}, 3},
		{"timeout not retried without 504", timeout, []int{502, 503}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			up := script(result{err: tt.err})
			r, _ := newRetry(up, config.BackoffFixed, tt.on...)
			if _, err := get(t, r); err == nil {
				t.Error("final error should be returned")
			}
			if up.Calls() != tt.wantCalls {
				t.Errorf("calls = %d, want %d", up.Calls(), tt.wantCalls)
			}
		})
	}
}

func TestRetryStopsWhenClientLeaves(t *testing.T) {
	up := script(result{status: 503})
	r, _ := newRetry(up, config.BackoffFixed, 503)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://upstream/", nil)
	if _, err := r.RoundTrip(req); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if up.Calls() != 1 {
		t.Errorf("calls = %d; no retries after the client has gone", up.Calls())
	}
}

func TestRetryReplaysBody(t *testing.T) {
	up := script(result{status: 503}, result{status: 503}, result{status: 200})
	r, _ := newRetry(up, config.BackoffFixed, 503)

	req, _ := http.NewRequest("POST", "http://upstream/", bytes.NewReader([]byte(`{"order":1}`)))
	if resp, err := r.RoundTrip(req); err != nil || resp.StatusCode != 200 {
		t.Fatalf("got %v %v", resp, err)
	}
	for i, b := range up.bodies {
		if b != `{"order":1}` {
			t.Errorf("attempt %d body = %q", i+1, b)
		}
	}
}
