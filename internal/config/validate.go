package config

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"
)

// Variables that may appear in transform values.
const (
	VarBody         = "$body"
	VarRequestTime  = "$request_time"
	VarResponseTime = "$response_time"
	VarRoutePath    = "$route_path"
	LiteralPrefix   = "$literal:"
)

var knownVariables = []string{VarBody, VarRequestTime, VarResponseTime, VarRoutePath}

// Upper bounds on values that are valid in form but would misbehave at
// runtime: amplify traffic, spin a CPU, or overflow arithmetic.
const (
	MaxRetryAttempts       = 10                     // each attempt is a full upstream call
	MaxRetryDelay          = 30 * time.Second       // also caps each exponential backoff step
	MaxTargetWeight        = 1000                   // keeps weighted round robin sums small
	MaxBreakerThreshold    = 10000                  // bounds the breaker's failure history
	MinHealthCheckInterval = 100 * time.Millisecond // probes every target each tick
)

var validMethods = []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "CONNECT", "TRACE"}

// Validate reports every problem in the config at once, each prefixed with the
// path of the offending field, so a broken config can be fixed in one pass.
// Zero values are accepted wherever a default exists.
func (c *Config) Validate() error {
	v := &validator{}
	v.gateway(&c.Gateway)
	if len(c.Routes) == 0 {
		v.add("routes", "at least one route is required")
	}
	seen := make(map[string]string)
	for i := range c.Routes {
		field := fmt.Sprintf("routes[%d]", i)
		v.route(field, &c.Routes[i])
		p := normalizePath(c.Routes[i].Path)
		if prev, dup := seen[p]; dup {
			v.add(field+".path", "%q duplicates %s.path", c.Routes[i].Path, prev)
		} else {
			seen[p] = field
		}
	}
	return errors.Join(v.errs...)
}

type validator struct{ errs []error }

func (v *validator) add(field, format string, args ...any) {
	v.errs = append(v.errs, fmt.Errorf("%s: %s", field, fmt.Sprintf(format, args...)))
}

func (v *validator) gateway(g *Gateway) {
	if g.Port < 0 || g.Port > 65535 {
		v.add("gateway.port", "must be between 1 and 65535, got %d", g.Port)
	}
	v.nonNegative("gateway.global_timeout", g.GlobalTimeout)
	if g.GlobalRateLimit != nil {
		v.rateLimit("gateway.global_rate_limit", g.GlobalRateLimit)
	}
}

func (v *validator) route(field string, r *Route) {
	if !strings.HasPrefix(r.Path, "/") {
		v.add(field+".path", "must start with \"/\", got %q", r.Path)
	}
	for i, m := range r.Methods {
		if !slices.Contains(validMethods, strings.ToUpper(m)) {
			v.add(fmt.Sprintf("%s.methods[%d]", field, i), "unknown HTTP method %q", m)
		}
	}
	v.upstream(field+".upstream", &r.Upstream)
	if r.Retry != nil {
		v.retry(field+".retry", r.Retry)
	}
	if r.RateLimit != nil {
		v.rateLimit(field+".rate_limit", r.RateLimit)
	}
	if r.HealthCheck != nil {
		v.healthCheck(field+".health_check", r.HealthCheck)
	}
	if r.RequestTransform != nil {
		v.transform(field+".request_transform", r.RequestTransform)
	}
	if r.ResponseTransform != nil {
		v.transform(field+".response_transform", r.ResponseTransform)
	}
	if r.Auth != nil {
		v.auth(field+".auth", r.Auth)
	}
	if r.CircuitBreaker != nil {
		v.circuitBreaker(field+".circuit_breaker", r.CircuitBreaker)
	}
}

func (v *validator) upstream(field string, u *Upstream) {
	switch {
	case u.URL == "" && len(u.Targets) == 0:
		v.add(field, "one of url or targets is required")
	case u.URL != "" && len(u.Targets) > 0:
		v.add(field, "url and targets are mutually exclusive")
	case u.URL != "":
		v.upstreamURL(field+".url", u.URL)
	}
	for i, t := range u.Targets {
		tf := fmt.Sprintf("%s.targets[%d]", field, i)
		v.upstreamURL(tf+".url", t.URL)
		if t.Weight < 0 || t.Weight > MaxTargetWeight {
			v.add(tf+".weight", "must be between 0 and %d, got %d", MaxTargetWeight, t.Weight)
		}
	}
	v.oneOf(field+".balance", u.Balance, BalanceRoundRobin, BalanceWeightedRoundRobin)
	v.nonNegative(field+".timeout", u.Timeout)
}

func (v *validator) upstreamURL(field, raw string) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		v.add(field, "must be an absolute http(s) URL, got %q", raw)
	}
}

func (v *validator) retry(field string, r *Retry) {
	if r.Attempts < 1 || r.Attempts > MaxRetryAttempts {
		v.add(field+".attempts", "must be between 1 and %d, got %d", MaxRetryAttempts, r.Attempts)
	}
	v.oneOf(field+".backoff", r.Backoff, BackoffFixed, BackoffExponential)
	v.nonNegative(field+".initial_delay", r.InitialDelay)
	if r.InitialDelay.Std() > MaxRetryDelay {
		v.add(field+".initial_delay", "must be at most %s", MaxRetryDelay)
	}
	for i, code := range r.On {
		if code < 100 || code > 599 {
			v.add(fmt.Sprintf("%s.on[%d]", field, i), "not an HTTP status code: %d", code)
		}
	}
}

func (v *validator) rateLimit(field string, rl *RateLimit) {
	if rl.Requests < 1 {
		v.add(field+".requests", "must be at least 1, got %d", rl.Requests)
	}
	v.positive(field+".window", rl.Window)
	v.oneOf(field+".strategy", rl.Strategy, StrategyFixedWindow, StrategySlidingWindow)
	v.oneOf(field+".per", rl.Per, PerIP, PerGlobal)
}

func (v *validator) healthCheck(field string, hc *HealthCheck) {
	if !strings.HasPrefix(hc.Path, "/") {
		v.add(field+".path", "must start with \"/\", got %q", hc.Path)
	}
	if hc.Interval.Std() < MinHealthCheckInterval {
		v.add(field+".interval", "must be at least %s", MinHealthCheckInterval)
	}
	if hc.UnhealthyThreshold < 0 {
		v.add(field+".unhealthy_threshold", "must not be negative, got %d", hc.UnhealthyThreshold)
	}
}

func (v *validator) auth(field string, a *Auth) {
	if a.Type != AuthAPIKey {
		v.add(field+".type", "must be %q, got %q", AuthAPIKey, a.Type)
	}
	v.headerName(field+".header", a.Header)
	if len(a.Keys) == 0 {
		v.add(field+".keys", "at least one key is required")
	}
	for i, k := range a.Keys {
		if k == "" {
			v.add(fmt.Sprintf("%s.keys[%d]", field, i), "must not be empty")
		}
	}
}

func (v *validator) circuitBreaker(field string, cb *CircuitBreaker) {
	if cb.Threshold < 1 || cb.Threshold > MaxBreakerThreshold {
		v.add(field+".threshold", "must be between 1 and %d, got %d", MaxBreakerThreshold, cb.Threshold)
	}
	v.positive(field+".window", cb.Window)
	v.positive(field+".cooldown", cb.Cooldown)
}

func (v *validator) transform(field string, t *Transform) {
	if h := t.Headers; h != nil {
		for name, value := range h.Add {
			f := field + ".headers.add." + name
			v.headerName(f, name)
			if strings.ContainsAny(value, "\r\n\x00") {
				v.add(f, "header value must not contain CR, LF or NUL")
			}
			v.variable(f, value, false)
		}
		for i, name := range h.Remove {
			v.headerName(fmt.Sprintf("%s.headers.remove[%d]", field, i), name)
		}
	}
	if b := t.Body; b != nil {
		v.mappingConflicts(field+".body.mapping", b.Mapping)
		for dest, src := range b.Mapping {
			f := field + ".body.mapping." + dest
			if !validDotPath(dest) {
				v.add(f, "destination must be a dot path like \"user.id\", got %q", dest)
			}
			if strings.HasPrefix(src, "$") {
				v.variable(f, src, false)
			} else if !validDotPath(src) {
				v.add(f, "source must be a dot path like \"userId\" or a $variable, got %q", src)
			}
		}
		v.envelope(field+".body.envelope", b.Envelope)
	}
}

// headerName requires an RFC 9110 token. An invalid name would otherwise pass
// startup and then fail every request on the route with a 502.
func (v *validator) headerName(field, name string) {
	if name == "" {
		v.add(field, "header name is required")
		return
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		isAlnum := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
		if !isAlnum && !strings.ContainsRune("!#$%&'*+-.^_`|~", rune(c)) {
			v.add(field, "invalid header name %q", name)
			return
		}
	}
}

// validDotPath accepts "a" and "a.b.c", rejecting empty segments ("a..b", ".a").
func validDotPath(p string) bool {
	return p != "" && !slices.Contains(strings.Split(p, "."), "")
}

// mappingConflicts rejects a destination that sits inside another one ("user"
// and "user.id"): the output would depend on map iteration order. Each
// destination's parents are checked directly, because a sorted-neighbour
// check misses cases like "user", "user-x", "user.id".
func (v *validator) mappingConflicts(field string, mapping map[string]string) {
	dests := make([]string, 0, len(mapping))
	for dest := range mapping {
		dests = append(dests, dest)
	}
	slices.Sort(dests) // stable error order
	for _, dest := range dests {
		parts := strings.Split(dest, ".")
		for i := 1; i < len(parts); i++ {
			if parent := strings.Join(parts[:i], "."); mapping[parent] != "" {
				v.add(field, "destinations %q and %q conflict: one is inside the other", parent, dest)
			}
		}
	}
}

func (v *validator) envelope(field string, node any) {
	switch n := node.(type) {
	case map[string]any:
		for k, child := range n {
			v.envelope(field+"."+k, child)
		}
	case []any:
		for i, child := range n {
			v.envelope(fmt.Sprintf("%s[%d]", field, i), child)
		}
	case string:
		v.variable(field, n, true)
	}
}

// variable rejects "$something" values that aren't a known variable, so a typo
// like "$request_tme" fails at startup rather than being sent as a literal.
// "$body" only has a meaning inside an envelope.
func (v *validator) variable(field, value string, inEnvelope bool) {
	if value == VarBody && !inEnvelope {
		v.add(field, "%s is only valid inside a body envelope", VarBody)
		return
	}
	if !strings.HasPrefix(value, "$") || strings.HasPrefix(value, LiteralPrefix) || slices.Contains(knownVariables, value) {
		return
	}
	v.add(field, "unknown variable %q (known: %s, %s<value>)", value, strings.Join(knownVariables, ", "), LiteralPrefix)
}

// oneOf accepts the empty string because every enum has a default.
func (v *validator) oneOf(field, value string, allowed ...string) {
	if value == "" || slices.Contains(allowed, value) {
		return
	}
	v.add(field, "must be one of %s, got %q", strings.Join(allowed, ", "), value)
}

func (v *validator) nonNegative(field string, d Duration) {
	if d < 0 {
		v.add(field, "must not be negative")
	}
}

func (v *validator) positive(field string, d Duration) {
	if d <= 0 {
		v.add(field, "is required and must be positive")
	}
}
