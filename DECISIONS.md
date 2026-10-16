# DECISIONS

> Working notes, kept up to date tier by tier. They get a final edit in the docs pass.

## Prioritization

Work is ordered in tiers, with one commit per tier, so the history shows the order and the reasoning.

| Tier | Contents | Why at this point |
|---|---|---|
| 0 | Config loading and validation, `/health`, routing (404/405), hand-written proxying, timeouts, `strip_prefix`, load balancing | The spec's non-negotiable requirements. Everything else plugs into this pipeline. Weighted round robin landed here because the example config uses it and the gateway has to boot on that file. |
| 1 | Rate limiting, API key auth, header transforms | Rate limiting under concurrency is called out by name in the grading. Auth and header transforms are cheap and show the pipeline is easy to extend. |
| 2 | Circuit breaker, retries | Resilience: what happens when an upstream is down or flaky. |
| 3 | Active health checks, body transforms | The most code and the most ambiguity for the least core value. |

## Architecture

Each route's pipeline is built **once at startup** as two decorator chains:

```
client → recover → access log (request id, request time, path cleaning) → /health → router (404/405)
  → auth → rate limit → request header transform → proxy.Handler            [http.Handler chain]
      → response header transform → Dispatch (balancer + per-attempt timeout) → shared http.Transport
                                                                              [http.RoundTripper chain]
```

- **Client-facing policies are `http.Handler` middleware.** These are policies that can reject a request before any upstream work is done.
- **Upstream-facing policies are `http.RoundTripper` decorators.** These are policies that need to see the upstream's response or error. They never write to the client, compose by wrapping, and are unit-testable with a fake transport.
- **`proxy.Handler` is the only bridge between the two chains**, and the only place where an error becomes a client response (through `httperr`). Every error has the same JSON shape: `{"error", "message", "retry_after"}`.
- **Adding a config feature** is a struct field, validation, one decorator, and one line in `buildRoute`.
- **Unbuilt features are reported, not hidden.** A feature that is configured but not implemented logs a warning at startup instead of being silently ignored.

## Decisions and trade-offs

**Config**
- **Strict decoding.** The YAML is decoded with `KnownFields(true)`, so typos fail at startup.
- **All errors at once.** `Validate()` collects every error, each with its field path, so a bad config is fixed in one pass. Exception: a YAML-level error such as an unparseable duration stops decoding at the first problem.
- **Unknown variables are errors.** A `$variable` the gateway doesn't recognize, such as `$request_tme`, is a validation error rather than being sent through as literal text.
- **Defaults are applied after validation.** Error messages therefore refer to what the user actually wrote, and later code never has to handle "unset".
- **Omitted fields:**
  - Omitting `methods` allows any method.
  - Omitting `weight` means 1.
  - Omitting `port` means 8080, and omitting `global_timeout` means 30s.

**Routing**
- **Matching is by longest prefix on path-segment boundaries.** `/api/users` matches `/api/users/1` but not `/api/usersX`.
- **Paths are cleaned before routing**, so `/public/../internal` can't bypass a route's policies.
- **`/health` is handled before routing** and can't be shadowed by a route.

**Proxying**
- **Built by hand** on `net/http`, without `httputil.ReverseProxy`.
- **Headers:**
  - Hop-by-hop headers are stripped, including any named in `Connection`.
  - The gateway adds `X-Forwarded-For`, `X-Forwarded-Host`, `X-Forwarded-Proto` and `X-Request-ID`.
- **Bodies stream** unless a feature needs them buffered.
- **Upstream errors:**
  - A connection failure returns 502, and a timeout returns 504.
  - A 5xx *from* the upstream is passed through unchanged, because it is the upstream's answer, not a gateway failure.
- **Timeouts are per attempt** and tied to the client's context, so a client that disconnects cancels the upstream call. There is no server `WriteTimeout`, because the route timeouts already bound slow upstreams.

**Load balancing**
- **Algorithm:** smooth weighted round robin (nginx's algorithm). Weights 3:1 give the sequence `a a b a`, not `a a a b`.

**Rate limiting**
- **Fixed window:** windows are aligned to the clock and counted per key.
- **Sliding window:** a sliding-window *counter*. The previous window's count is weighted by how much of it still overlaps the trailing window. It is approximate, but uses O(1) memory per key instead of O(limit) for a timestamp log, which matters when the key is a client IP.
- **The check and the increment happen under one mutex.** With 50 concurrent requests and a limit of 10, exactly 10 get through. This is tested end to end under `-race`.
- **`per: ip` uses the TCP peer address and ignores `X-Forwarded-For`.** A client can set that header to anything, which would give it a fresh bucket each time. Trusting the header safely needs a configured list of trusted proxies.
- **`global_rate_limit` is a default policy, not a shared quota.** Each route that doesn't override it gets its own copy and its own buckets.
- **Rejected requests** get 429, a `Retry-After` header, and `retry_after` in the body. For the sliding window, `retry_after` is solved exactly from the weighted estimate and rounded up.
- **Memory:** a janitor goroutine removes idle buckets so one-off IPs don't build up. Remaining risk: within one window, memory still grows with the number of distinct IPs. Sharding the map or capping the number of buckets would be next.

**Auth (`api_key`)**
- **Responses:** a missing or invalid key returns 401 with a JSON body.
- **Comparison:** keys are compared in constant time against *every* configured key. One remaining leak: `ConstantTimeCompare` returns early when the lengths differ.
- **The key header is stripped before forwarding**, so upstreams never see gateway credentials.
- **Auth runs before rate limiting**, so unauthenticated traffic can't use up a `per: global` quota. The counter-argument is that limiting first slows down key brute-forcing; with `per: ip` that risk is small.

**Header transforms**
- **Order:** removes are applied before adds, so an add can't be wiped out by a remove of the same name.
- **`$request_time`** is captured once, at the edge, and stored in the request context, so every transform sees the same timestamp. It is formatted as RFC 3339 in UTC with millisecond precision.
- **Response transforms apply only to upstream responses.** Gateway-generated errors (429, 502, ...) are never altered.

## Partially implemented / not yet built

- Retries, circuit breaker, active health checks, and body transforms (mapping and envelope). Each logs a startup warning when configured.

## With more time

- Flush streamed responses (SSE) as they arrive. Support WebSocket/Upgrade proxying.
- Trusted-proxy configuration for `X-Forwarded-For`.
- Hot reload of the config on SIGHUP.
- Metrics (per-route latency, status counts, limiter rejections).
