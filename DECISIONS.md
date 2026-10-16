# DECISIONS

> Working notes, kept up to date tier by tier. They get a final edit in the docs pass.

## Prioritization

Work is ordered in tiers, with one commit per tier, so the history shows the order and the reasoning.

| Tier | Contents | Why at this point |
|---|---|---|
| 0 | Config loading and validation, `/health`, routing (404/405), hand-written proxying, timeouts, `strip_prefix`, load balancing | The spec's non-negotiable requirements. Everything else plugs into this pipeline. Weighted round robin landed here because the example config uses it and the gateway has to boot on that file. |
| 1 | Rate limiting, API key auth, header transforms | Rate limiting under concurrency is called out by name in the grading. Auth and header transforms are cheap and show the pipeline is easy to extend. |
| 2 | Circuit breaker, retries | Resilience: what happens when an upstream is down or flaky. Tiers 0–1 answer it with a clean 502/504, but keep sending traffic to a dead upstream and give up on the first transient error. |
| 3 | Active health checks, body transforms | The most code and the most ambiguity for the least core value. |

## Architecture

Each route's pipeline is built **once at startup** as two decorator chains:

```
client → recover → access log (request id, request time, path cleaning) → /health → router (404/405)
  → auth → rate limit → request header transform → proxy.Handler                       [http.Handler chain]
      → response header transform → circuit breaker → retry → Dispatch (balancer + per-attempt timeout)
      → shared http.Transport                                                           [http.RoundTripper chain]
```

- **Client-facing policies are `http.Handler` middleware.** These are policies that can reject a request before any upstream work is done.
- **Upstream-facing policies are `http.RoundTripper` decorators.** These are policies that need to see the upstream's response or error. They never write to the client, compose by wrapping, and are unit-testable with a fake transport.
- **The order of the upstream-facing chain matters:**
  - **The breaker sits above retry.** One client request counts as at most one failure, and an open breaker rejects without spending time on retries.
  - **Retry sits above `Dispatch`.** Each attempt picks a target again, so a retry can fail over to a healthy one, and each attempt gets its own timeout.
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

**Circuit breaker**
- **What counts as a failure:**
  - A connection error, a timeout, or a 5xx response is a failure.
  - A 4xx is the client's fault and doesn't count.
  - A request the client abandoned doesn't count.
- **"5 failures within 60s" is a rolling window, not "5 in a row".** Successes don't reset the count; old failures simply age out. Only the last `threshold` failure timestamps are kept, so memory is bounded.
- **While open,** the breaker fails fast without calling the upstream, with exactly the spec's body, `{"error":"service_unavailable","retry_after":<seconds left>}`, plus a `Retry-After` header.
- **Half-open lets exactly one probe through.** Concurrent requests are still rejected (`retry_after: 1`) until the probe returns, so a recovering upstream isn't hit by a thundering herd. If the probe succeeds the breaker closes and its history is cleared; if it fails the breaker re-opens for a full cooldown.
- **One breaker per route**, because that's where the config puts it. With several targets, a per-target breaker would be more precise.
- **State changes are logged** at WARN.

**Retries**
- **`attempts` is the total**, including the first try. `attempts: 3` means 1 original plus 2 retries.
- **Which failures are retried.** Gateway-side failures are mapped to the status the client would get: a connection failure counts as 502 and a timeout as 504. They're retried only if that code is in `on`, so every retry is driven by config with no hidden rules.
- **Backoff:**
  - `fixed` waits `initial_delay` each time.
  - `exponential` waits `initial_delay × 2^(n−1)`, so 1s then 2s for `/api/orders`.
  - Waits abort the moment the client disconnects.
  - There is no jitter, so the timing matches the config exactly. Jitter is listed under next steps.
- **Bodies are buffered on routes with `retry`**, capped at 10 MB with a 413 above that, so every attempt can re-send them. Other routes stream.
- **Discarded responses are drained and closed** so their connections are reused.
- **When attempts run out, the client gets the upstream's real last response**, for example its actual 503, rather than a gateway-generated error.
- **Worst-case latency is attempts × timeout + backoff.** For `/api/orders` that's 3 × 5s + 3s ≈ 18s. An overall per-request deadline would be the next addition.

### Decision: retries follow the config, even for POST and PUT

`/api/orders` enables `retry` on a route that allows POST and PUT. Retrying a non-idempotent request after a 5xx can **perform it twice**, for example creating a duplicate order if the upstream committed before failing. Two options were considered:

- **(a) Honor the config as written.** ✅ Chosen.
- (b) Retry only idempotent methods, plus POSTs that failed before the connection was made.

We chose **(a)**. Whoever wrote the config turned on retries for that route explicitly, and a gateway that quietly overrides its config is harder to reason about than one that does exactly what it was told. The risk sits with the config: enabling `retry` on a route amounts to saying its upstream is idempotent. The proper fix is end-to-end `Idempotency-Key` support (pass a client key through, or generate one per request and reuse it on every retry) so the upstream can drop duplicates. That's listed under next steps.

## Partially implemented / not yet built

- Active health checks and body transforms (mapping and envelope). Each logs a startup warning when configured.

## With more time

- Flush streamed responses (SSE) as they arrive. Support WebSocket/Upgrade proxying.
- Trusted-proxy configuration for `X-Forwarded-For`.
- `Idempotency-Key` support so POST retries are safe. Retry jitter. Honoring upstream `Retry-After`. An overall per-request deadline across retries.
- Per-target circuit breakers on load-balanced routes.
- Hot reload of the config on SIGHUP.
- Metrics (per-route latency, status counts, limiter rejections).
