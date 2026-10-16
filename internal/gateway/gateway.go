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
	}

	g.handler = g.recoverPanics(g.accessLog(g.normalizePath(g.withHealth(router.New(routes)))))
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
	logger := g.logger.With("route", rc.Path)
	if rc.HealthCheck != nil {
		hc := upstream.NewHealthChecker(*rc.HealthCheck, targets, g.transport, rc.Upstream.Timeout.Std(), logger)
		go hc.Run(g.ctx)
	}

	// Upstream-facing chain, built innermost first, so it runs:
	// response headers -> response body -> circuit breaker -> retry -> Dispatch -> transport.
	var rt http.RoundTripper = &upstream.Dispatch{
		Balancer: balancer,
		Timeout:  rc.Upstream.Timeout.Std(),
		Next:     g.transport,
	}
	// Retry sits above Dispatch so each attempt re-picks a target and gets
	// its own timeout.
	if rc.Retry != nil {
		rt = upstream.NewRetry(*rc.Retry, rt, logger)
	}
	// The breaker sits above retry: one client request counts as at most one
	// failure, and an open breaker rejects without spending time on retries.
	if rc.CircuitBreaker != nil {
		rt = upstream.NewBreaker(*rc.CircuitBreaker, rt, logger)
	}
	// Body before headers, so the config's header rules have the final say
	// (e.g. over the Content-Type the envelope sets).
	if t := rc.ResponseTransform; t != nil && t.Body != nil {
		rt = transform.ResponseBody(t.Body, rc.Path, logger, rt)
	}
	if t := rc.ResponseTransform; t != nil && t.Headers != nil {
		rt = transform.ResponseHeaders(t.Headers, rc.Path, rt)
	}

	// Client-facing chain, built innermost first, so it runs:
	// auth -> rate limit -> request headers -> request body -> proxy.
	var h http.Handler = &proxy.Handler{
		Transport:   rt,
		StripPrefix: stripPrefix(rc),
		// Retries must be able to re-send the body, so buffer it.
		BufferBody: rc.Retry != nil,
		Logger:     logger,
	}
	if t := rc.RequestTransform; t != nil && t.Body != nil {
		h = transform.RequestBody(t.Body, rc.Path)(h)
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
	// Outermost: bound how long the client may take to send its body, so a
	// slow sender can't hold the request open (the route timeout only starts
	// with the upstream call).
	h = middleware.BodyReadTimeout(rc.Upstream.Timeout.Std())(h)
	return tagRoute(rc.Path, h), nil
}

func stripPrefix(rc *config.Route) string {
	if rc.StripPrefix {
		return rc.Path
	}
	return ""
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
