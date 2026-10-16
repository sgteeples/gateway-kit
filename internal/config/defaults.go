package config

import (
	"strings"
	"time"
)

const (
	DefaultPort               = 8080
	DefaultGlobalTimeout      = 30 * time.Second
	DefaultRetryDelay         = 100 * time.Millisecond
	DefaultUnhealthyThreshold = 3
)

// DefaultRetryOn is used when a retry block omits "on".
var DefaultRetryOn = []int{502, 503, 504}

// applyDefaults normalizes a validated config so the rest of the gateway never
// has to handle "unset": every route ends up with targets, a balance strategy,
// a timeout and (if a global one exists) a rate limit.
func (c *Config) applyDefaults() {
	g := &c.Gateway
	if g.Port == 0 {
		g.Port = DefaultPort
	}
	if g.GlobalTimeout == 0 {
		g.GlobalTimeout = Duration(DefaultGlobalTimeout)
	}
	if g.GlobalRateLimit != nil {
		g.GlobalRateLimit.applyDefaults()
	}
	for i := range c.Routes {
		c.Routes[i].applyDefaults(g)
	}
}

func (r *Route) applyDefaults(g *Gateway) {
	r.Path = normalizePath(r.Path)
	for i, m := range r.Methods {
		r.Methods[i] = strings.ToUpper(m)
	}

	u := &r.Upstream
	if u.URL != "" {
		u.Targets = []Target{{URL: u.URL, Weight: 1}}
	}
	for i := range u.Targets {
		if u.Targets[i].Weight == 0 {
			u.Targets[i].Weight = 1
		}
	}
	if u.Balance == "" {
		u.Balance = BalanceRoundRobin
	}
	if u.Timeout == 0 {
		u.Timeout = g.GlobalTimeout
	}

	switch {
	case r.RateLimit != nil:
		r.RateLimit.applyDefaults()
	case g.GlobalRateLimit != nil:
		// Each route gets its own copy, and so its own buckets: the global
		// limit is a default policy, not a shared quota across routes.
		rl := *g.GlobalRateLimit
		r.RateLimit = &rl
	}

	if rt := r.Retry; rt != nil {
		if rt.Backoff == "" {
			rt.Backoff = BackoffFixed
		}
		if rt.InitialDelay == 0 {
			rt.InitialDelay = Duration(DefaultRetryDelay)
		}
		if len(rt.On) == 0 {
			rt.On = DefaultRetryOn
		}
	}
	if hc := r.HealthCheck; hc != nil && hc.UnhealthyThreshold == 0 {
		hc.UnhealthyThreshold = DefaultUnhealthyThreshold
	}
}

func (rl *RateLimit) applyDefaults() {
	if rl.Strategy == "" {
		rl.Strategy = StrategyFixedWindow
	}
	if rl.Per == "" {
		rl.Per = PerIP
	}
}

// normalizePath drops trailing slashes so "/api/users/" and "/api/users" are
// the same route.
func normalizePath(p string) string {
	if len(p) <= 1 {
		return p
	}
	if p = strings.TrimRight(p, "/"); p == "" {
		return "/"
	}
	return p
}
