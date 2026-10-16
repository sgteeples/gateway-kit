# DECISIONS

GatewayKit implements **every feature in the config schema**. It was built in four priority tiers, with one commit per tier, so the git history follows the same order as this document. Run `make test` to check it.

## How I prioritized

Each tier had to be finished, tested and pushed before the next one started. That way, stopping at any point would still have left a working gateway.

**Tier 0: Core requirements.** The spec's non-negotiables, and the foundation everything else plugs into.
- Loading the config, with strict validation that reports every error at once
- `GET /health`
- Routing: longest-prefix match, 404 and 405
- Proxying written by hand: headers, bodies, `strip_prefix`, timeouts, 502/504
- Load balancing: round robin and weighted. This was pulled forward from Tier 2 because the example config uses it, and the gateway has to start on that config.

**Tier 1: Features the grading names directly.** The rubric asks what happens when "50 requests hit a rate-limited route simultaneously".
- Rate limiting: fixed and sliding window, keyed by IP or shared globally, with a correctness test under concurrency
- API key auth
- Header transforms. They are cheap, and they show how easily the pipeline extends.

**Tier 2: Resilience.** Tiers 0 and 1 answer "the upstream is down" with a clean 502, but still send traffic to the dead upstream.
- Circuit breaker
- Retries with backoff, including failing over to another target

**Tier 3: Most code, most ambiguity, least core value.**
- Active health checks. They only change *which* target is chosen, and retries and the breaker already cope with a dead one.
- Body transforms: request mapping and response envelope

## Key decisions

| Question | Chosen | Alternative | Why |
|---|---|---|---|
| Retry POST/PUT? | Follow the config | Retry only idempotent methods | The config author enabled retries explicitly, and a gateway that overrides its config is harder to reason about. The duplicate-write risk is real; `Idempotency-Key` support is next. |
| Every target unhealthy? | Fail open (try them all) | Return 503 | A broken health endpoint shouldn't take down a route. Envoy's "panic threshold" works the same way. |
| What does body mapping output? | Only the mapped fields | Merge into the original body | "Restructure" implies the upstream expects the new shape. Merging would send `userId` and `user.id` side by side. |
| Which responses get the envelope? | Every upstream response, status kept | 2xx only | Clients get one shape. The gateway's own errors are never wrapped. |
| Auth or rate limit first? | Auth | Rate limit | Unauthenticated traffic can't use up a `per: global` quota. |
| Rate-limit key for `per: ip` | TCP peer address | `X-Forwarded-For` | A client can set that header to anything and get a fresh bucket each time. |
| Unknown config keys? | Reject at startup | Warn and ignore | The YAML must match the schema exactly. Otherwise a typo like `retyr:` would silently disable a feature. |
| Ambiguous request paths (`;`, `%2F`, `%5C`, control characters) | Reject with 400 | Forward as-is | The router and the upstream must see the same path. Otherwise `/api/internal;x` skips the auth route and a Java upstream serves `/api/internal` anyway. |

Interpretations of the spec's ambiguous parts:
- **Retries:** `attempts` is the total number of tries. A connection failure counts as 502 and a timeout as 504 when checking the `on` list.
- **Circuit breaker:** the window is rolling ("5 failures within 60s", not "5 in a row"). A 4xx doesn't count as a failure.
- **Health checks:** a target recovers after one successful check.
- **Global rate limit:** a default policy. Each route gets its own counters, not a share of one quota.

## Production thinking

- **When an upstream is down:**
  - The client gets a 502, or a 504 on timeout.
  - The circuit breaker trips, and requests then fail fast without reaching the upstream.
  - Retries fail over to another target.
  - Health checks steer traffic away from it.
- **When the config is malformed:** every problem is reported at startup with its field path, and the gateway exits non-zero. Validation also rejects values that are valid in form but would break at runtime:
  - invalid header names, or CR/LF in header values
  - unknown or misplaced `$variables`
  - conflicting mapping paths
  - out-of-range values: `attempts` above 10, `weight` above 1000, a health-check interval under 100ms
- **When 50 requests arrive at once:** the rate-limit check and the counter update happen under one mutex. Exactly N requests get through, which is tested end to end under `-race`.
- **When a client disconnects:** the upstream call and any retry waits are cancelled.
- **When a client is slow:** the request body must arrive within the route timeout, or the client gets 408. The deadline is lifted once the body has arrived, so it never cuts short retries.
- **When an upstream dies mid-response:** the client connection is aborted rather than finished cleanly, so a truncated body can't pass for a complete one.
- **Memory stays bounded:**
  - Idle rate-limit buckets are swept.
  - Buffered bodies are capped at 10 MB.
  - The breaker keeps at most `threshold` timestamps.
- **On shutdown:** SIGTERM drains in-flight requests and stops the background goroutines.

## Known limitations

1. **CONNECT and TRACE are forwarded** on routes with no `methods` list; they should get a 405. Go 1.27.2 also patches standard-library HTTP vulnerabilities, so the toolchain should be upgraded.
2. **No streaming:** responses aren't flushed as they arrive (no SSE), WebSockets aren't supported, and envelope routes buffer the whole response.
3. **Failed logins aren't rate limited:** auth runs before the rate limiter, so with `per: ip`, key guessing is unthrottled.

## What I'd build next

1. **`Idempotency-Key` support**, so retrying POST is safe.
2. **Passive health checks and per-target breakers**, so real traffic failures feed health.
3. **Metrics** for each route: latency, status codes, rate-limit rejections, breaker state.
4. **Hot reload** of the config on SIGHUP.
5. **Proxy features:** flushing streamed responses, WebSocket proxying, a trusted-proxy list for `X-Forwarded-For`, and retry jitter.

## How I used AI tools

I built this with **Claude Code** in the terminal.

- **Plan mode:** Claude Code read the spec and drafted an implementation plan: the package layout, the two-chain pipeline and the priority tiers. I reviewed and adjusted the plan before any code was written. It then served as the outline for each tier.
- **Implementation:** before each tier, I had the design explained to me so I could make the judgment calls listed in *Key decisions*. Claude Code then wrote the code and tests for the tier.
- **Verification:** nothing was committed on the strength of "it compiles."
  - Every tier ran the full suite under Go's race detector.
  - The concurrency and timing tests were run 20 times in a row to catch flaky results.
  - The real gateway was run against `gateway.yaml` and the mock upstreams, and checked with `curl`.
- **What verification caught:**
  - **A sliding-window bug:** `retry_after` was a fraction of a millisecond too early, because of floating-point rounding.
  - **A weak test:** a weighted-round-robin test was too loose, and `go vet` flagged it.
  - **A misplaced test failure:** a test helper called `t.Fatalf` from a goroutine other than the test's own, which Go doesn't allow.
  - **A wrong README example:** it claimed a 5s timeout, but the request is actually retried and takes about 18s.

### Independent review agents

Once the build was finished, I ran separate Claude Code agents to check the work. Each started with **no context from the build session**, so it couldn't inherit my assumptions. They were read-only: they could run the code, but every fix went through me. They ran in parallel, each on its own ports.

| Agent | What it did | Outcome |
|---|---|---|
| **Spec audit** | Checked every requirement and deliverable in the PDF literally. It wrote its own schema-valid configs and ran the real binary against them, to stand in for the graders' "different config". | Verdict: ready to submit. Found that the body mapping was skipped for JSON sent without `Content-Type: application/json` (e.g. plain `curl -d`), which is now fixed. It also flagged that strict config rejects unknown keys; I kept that deliberately (see *Key decisions*). |
| **Code review** | Reviewed the code for robustness, security and Go gateway practice, comparing the hand-written proxy against what `httputil.ReverseProxy` handles. It ran `go vet`, `staticcheck` and `govulncheck`, and proved each high-severity finding by reproducing it. | Four findings were fixed, each reproduced first and then covered by a test: truncated responses passing as complete, slow request bodies with no time limit, an auth bypass via path tricks, and config values that passed validation but broke at runtime. The rest are listed under *Known limitations*. |
| **Architecture explainer** | Read the code and built an interactive page showing the request pipeline, the startup flow, every component, and why the chain is in its order. | Used to prepare for the walkthrough. |
| **Manual testing guide** | Wrote a copy-paste command guide for every route, with success and failure cases, and ran every command against the real binary to record actual output. | Used to rehearse running the gateway by hand during the walkthrough. |
