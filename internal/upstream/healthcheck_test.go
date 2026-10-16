package upstream

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sgteeples/gateway-kit/internal/config"
	"github.com/sgteeples/gateway-kit/internal/mockupstream"
)

func newChecker(t *testing.T, threshold int) (*HealthChecker, *mockupstream.Server, *Target) {
	t.Helper()
	mock := mockupstream.New("a")
	srv := httptest.NewServer(mock)
	t.Cleanup(srv.Close)
	target, _ := NewTarget(srv.URL, 1)
	hc := NewHealthChecker(config.HealthCheck{
		Path:               "/healthz",
		Interval:           config.Duration(time.Hour), // ticks are driven by hand
		UnhealthyThreshold: threshold,
	}, []*Target{target}, http.DefaultTransport, time.Second, discardLogger)
	return hc, mock, target
}

func TestHealthCheckThresholdAndRecovery(t *testing.T) {
	hc, mock, target := newChecker(t, 3)
	ctx := context.Background()

	mock.SetHealthy(false)
	for i := 1; i <= 2; i++ {
		hc.checkAll(ctx)
		if !target.Healthy() {
			t.Fatalf("marked unhealthy after %d failures; threshold is 3", i)
		}
	}
	hc.checkAll(ctx)
	if target.Healthy() {
		t.Fatal("should be unhealthy after 3 consecutive failures")
	}

	mock.SetHealthy(true)
	hc.checkAll(ctx)
	if !target.Healthy() {
		t.Error("one successful probe should restore the target")
	}
}

func TestHealthCheckNeedsConsecutiveFailures(t *testing.T) {
	hc, mock, target := newChecker(t, 2)
	ctx := context.Background()

	mock.SetHealthy(false)
	hc.checkAll(ctx)
	mock.SetHealthy(true)
	hc.checkAll(ctx) // resets the streak
	mock.SetHealthy(false)
	hc.checkAll(ctx)
	if !target.Healthy() {
		t.Error("failures interrupted by a success must not add up")
	}
}

func TestHealthCheckUnreachableTarget(t *testing.T) {
	target, _ := NewTarget("http://127.0.0.1:1", 1) // nothing listens on port 1
	hc := NewHealthChecker(config.HealthCheck{Path: "/healthz", Interval: config.Duration(time.Hour), UnhealthyThreshold: 1},
		[]*Target{target}, http.DefaultTransport, time.Second, discardLogger)
	hc.checkAll(context.Background())
	if target.Healthy() {
		t.Error("an unreachable target should be marked unhealthy")
	}
}

func TestHealthCheckStopsOnCancel(t *testing.T) {
	hc, _, _ := newChecker(t, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		hc.Run(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}
