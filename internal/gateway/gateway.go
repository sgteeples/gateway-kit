// Package gateway assembles the request pipeline from config.
//
// For every route it builds two chains once at startup:
//
//	client-facing (http.Handler):        [policies that can reject early] -> proxy.Handler
//	upstream-facing (http.RoundTripper): [policies around the call]       -> Dispatch -> shared Transport
//
// Adding a config feature means writing one decorator and wiring it in
// buildRoute. No per-request config lookups happen on the hot path.
package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/sgteeples/gateway-kit/internal/config"
	"github.com/sgteeples/gateway-kit/internal/httperr"
	"github.com/sgteeples/gateway-kit/internal/middleware"
	"github.com/sgteeples/gateway-kit/internal/proxy"
	"github.com/sgteeples/gateway-kit/internal/ratelimit"
	"github.com/sgteeples/gateway-kit/internal/router"
	"github.com/sgteeples/gateway-kit/internal/transform"
	"github.com/sgteeples/gateway-kit/internal/upstream"
)

// Gateway is the top-level http.Handler.
type Gateway struct {
	handler   http.Handler
	started   time.Time
	logger    *slog.Logger
	transport *http.Transport

	// ctx is cancelled by Close to stop background goroutines.
	ctx    context.Context
	cancel context.CancelFunc
}

// New builds a gateway from a validated config. Call Close when done to stop
// background work.
func New(cfg *config.Config, logger *slog.Logger) (*Gateway, error) {
	g := &Gateway{
		started:   time.Now(),
		logger:    logger,
		transport: newTransport(),
	}
	g.ctx, g.cancel = context.WithCancel(context.Background())

	routes := make([]router.Route, 0, len(cfg.Routes))
	for i := range cfg.Routes {
		rc := &cfg.Routes[i]
		h, err := g.buildRoute(rc)
		if err != nil {
			g.Close()
			return nil, fmt.Errorf("route %s: %w", rc.Path, err)
		}
		routes = append(routes, router.Route{Path: rc.Path, Methods: rc.Methods, Handler: h})
		for _, feature := range unimplemented(rc) {
			logger.Warn("config feature is not implemented yet and will be ignored", "route", rc.Path, "feature", feature)
		}
	}

	g.handler = g.recoverPanics(g.accessLog(g.withHealth(router.New(routes))))
	return g, nil
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.handler.ServeHTTP(w, r)
}

// Close stops background goroutines and releases idle upstream connections.
func (g *Gateway) Close() {
	g.cancel()
	g.transport.CloseIdleConnections()
}

func (g *Gateway) buildRoute(rc *config.Route) (http.Handler, error) {
	targets := make([]*upstream.Target, 0, len(rc.Upstream.Targets))
	for _, tc := range rc.Upstream.Targets {
		t, err := upstream.NewTarget(tc.URL, tc.Weight)
		if err != nil {
			return nil, err
		}
		targets = append(targets, t)
	}
	balancer, err := upstream.NewBalancer(rc.Upstream.Balance, targets)
	if err != nil {
		return nil, err
	}

	// Upstream-facing chain, built innermost first:
	// response transform -> Dispatch -> shared transport.
	var rt http.RoundTripper = &upstream.Dispatch{
		Balancer: balancer,
		Timeout:  rc.Upstream.Timeout.Std(),
		Next:     g.transport,
	}
	if t := rc.ResponseTransform; t != nil && t.Headers != nil {
		rt = transform.ResponseHeaders(t.Headers, rc.Path, rt)
	}

	// Client-facing chain, built innermost first, so it runs:
	// auth -> rate limit -> request transform -> proxy.
	var h http.Handler = &proxy.Handler{
		Transport:   rt,
		StripPrefix: stripPrefix(rc),
		Logger:      g.logger.With("route", rc.Path),
	}
	if t := rc.RequestTransform; t != nil && t.Headers != nil {
		h = transform.RequestHeaders(t.Headers, rc.Path)(h)
	}
	if rl := rc.RateLimit; rl != nil {
		limiter := ratelimit.New(*rl, time.Now)
		go limiter.RunJanitor(g.ctx)
		h = middleware.RateLimit(limiter, rl.Per)(h)
	}
	// Auth runs before rate limiting so unauthenticated traffic can't use up
	// a `per: global` quota meant for real callers.
	if a := rc.Auth; a != nil {
		h = middleware.APIKey(a.Header, a.Keys)(h)
	}
	return h, nil
}

func stripPrefix(rc *config.Route) string {
	if rc.StripPrefix {
		return rc.Path
	}
	return ""
}

// unimplemented lists features present in the route config that this build
// does not enforce, so they are reported at startup instead of silently ignored.
func unimplemented(rc *config.Route) []string {
	var out []string
	if rc.Retry != nil {
		out = append(out, "retry")
	}
	if rc.CircuitBreaker != nil {
		out = append(out, "circuit_breaker")
	}
	if rc.HealthCheck != nil {
		out = append(out, "health_check")
	}
	if t := rc.RequestTransform; t != nil && t.Body != nil {
		out = append(out, "request_transform.body")
	}
	if t := rc.ResponseTransform; t != nil && t.Body != nil {
		out = append(out, "response_transform.body")
	}
	return out
}

// newTransport is the single connection pool shared by every route.
func newTransport() *http.Transport {
	return &http.Transport{
		Proxy: nil, // never send upstream traffic through an HTTP_PROXY from the environment
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          512,
		MaxIdleConnsPerHost:   64,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
	}
}

func (g *Gateway) withHealth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			next.ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			httperr.Write(w, &httperr.Error{Status: http.StatusMethodNotAllowed, Code: "method_not_allowed"})
			return
		}
		httperr.WriteJSON(w, http.StatusOK, map[string]any{
			"status":         "healthy",
			"uptime_seconds": int(time.Since(g.started).Seconds()),
		})
	})
}
