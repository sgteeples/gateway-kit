# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project

GatewayKit: a config-driven API gateway written in Go for a 2-hour take-home. The behaviour is specified by `gateway.yaml`. It is also graded against *other* configs with the same schema, so nothing may depend on the example routes, ports or values.

Hard constraint: only the standard library plus `gopkg.in/yaml.v3`. **No proxy, reverse-proxy or gateway libraries, and that includes `net/http/httputil.ReverseProxy`.** The forwarding logic is built by hand in `internal/proxy`.

## Commands

```sh
make test                     # go test -race ./... (the full suite; the single command graders run)
go test -race ./internal/gateway -run TestUpstreamTimeoutIs504   # a single test
make vet                      # go vet ./...
make mock                     # mock upstreams on :3001-3006 for the example config
make run                      # go run ./cmd/gateway gateway.yaml (CONFIG=path to override)
```

Config path resolution: `-config` flag, then the first positional arg, then `$GATEWAY_CONFIG`, then `./gateway.yaml`.

## Architecture

Each route's pipeline is built **once at startup** by `internal/gateway.buildRoute`, as two decorator chains:

- **Client-facing `http.Handler` chain:** policies that can reject or rewrite a request before any upstream call (auth → rate limit → request headers → request body). It ends in `proxy.Handler`.
- **Upstream-facing `http.RoundTripper` chain:** policies that must see the upstream response or error (response headers → response body → circuit breaker → retry → `upstream.Dispatch` → shared `http.Transport`). These never write to the client.
- **Background work:** one `upstream.HealthChecker` per route that has a `health_check`. It flips each `Target`'s atomic healthy flag, and the balancers read that flag. Rate-limit janitors also run in the background. All of it stops when `Gateway.Close()` cancels `g.ctx`.

`proxy.Handler` is the bridge between the two. It builds the outbound request with only the path and query set. `upstream.Dispatch` then picks a target from the `Balancer`, sets the scheme and host, and applies the per-attempt route timeout. Dispatch sits *below* retry, so each attempt can go to a different target and gets its own timeout.

All error responses go through `internal/httperr`. Upstream-facing layers return `*httperr.Error` as a plain Go error, and `proxy.Handler` is the one place that turns errors into responses. `httperr.From` maps timeouts to 504 and connection failures to 502.

Config flow: `config.Parse` decodes with `KnownFields(true)`, then `Validate()` collects **all** errors with field paths, then `applyDefaults()` normalizes. After normalization:
- every route has `Upstream.Targets`, since a single `url` becomes one target
- every route has a balance strategy and a timeout
- every route has its own *copy* of the global rate limit if it didn't define one

Downstream code never has to handle unset values.

**Adding a config feature:**
1. Add the field to `internal/config/config.go`.
2. Add validation in `validate.go` and defaults in `defaults.go`.
3. Write the decorator in the matching package.
4. Wire it into `buildRoute`, at the right position in the chain. The order is documented inline there and in `DECISIONS.md`.

`/health` is handled before routing and always wins. Request paths are cleaned (`.` and `..` resolved) before routing, so they can't bypass a route's policies.

## Testing conventions

- End-to-end tests are in `internal/gateway/gateway_test.go`. They start `internal/mockupstream` servers with `httptest`, then build a gateway from inline YAML that uses those servers' real URLs.
- The mock upstream echoes the request back as JSON (`mockupstream.Echo`). It supports `?status=`, `?delay=` and `?fail_rate=`, plus `FailNext(n, status)` and `SetHealthy` for tests.
- Prefer an injected clock over sleeps for anything time-based.

## Workflow

Make one commit per priority tier (not per feature), with a body explaining what's in the tier and why it comes at that point. Push after each tier. Update `DECISIONS.md` (prioritization, trade-offs, partial features) and the README feature checklist as features land.
