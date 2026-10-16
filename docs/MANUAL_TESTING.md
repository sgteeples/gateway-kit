# GatewayKit: Manual Testing Guide

A hands-on guide to running GatewayKit by hand against `gateway.yaml` and the mock upstreams, and to walking an interviewer through each route.

**How this guide was verified:** unless a step is marked _not run_, every command was run against binaries built from commit `18734ee` (`make build`) on macOS with Go 1.27.1 and curl 8.7.1. The [hardening checks](#hardening-checks) in section 3 were added and verified later, against commit `6efc6fd`. The outputs are real but trimmed. Request IDs, timestamps, `uptime_seconds` and `Retry-After` values will be different on your machine.

> **Rate limits carry over between sections.** `/api/users` allows 30 requests per 60s and `/api/orders` allows 10 per 10s, per IP. Every other route uses the global default of 100 per 60s. If a success case gives you an unexpected 429, wait out the `Retry-After` value, or restart the gateway to reset every counter.

---

## 1. Setup

### Terminals

Run all commands from the repo root.

```sh
# Terminal 1: mock upstreams on :3001-3006 (one process for all six ports)
make mock                    # or: go run ./cmd/mockupstream
# mock upstream listening on :3001
# ...
# mock upstream listening on :3006

# Terminal 2: gateway on :8080, with its JSON logs saved to a file
go run ./cmd/gateway gateway.yaml 2> gateway.log
#   (or: make run   to log straight to the terminal)

# Terminal 3: watch the logs
tail -f gateway.log | jq -c .                                  # with jq
tail -f gateway.log | jq -c 'select(.msg != "request")'        # events only: retries, breaker, health
tail -f gateway.log                                            # without jq
tail -f gateway.log | grep -v '"msg":"request"'                # events only, without jq

# Terminal 4: run the curl commands from here
```

On startup, the gateway logs:
```json
{"time":"...","level":"INFO","msg":"gateway listening","addr":":8080","config":"gateway.yaml","routes":5}
```

_Verification note:_ the outputs here came from the built binaries (`./bin/mockupstream`, `./bin/gateway gateway.yaml`), which run the same code as `make mock` and `make run`. The `tail -f | jq` pipelines were not run as live streams; the logs were read with `tail` and `grep` on the file instead.

### Stopping just one mock port

`make mock` runs all six ports in a single process, so you can't stop just one of them. For the "upstream down" tests, run the port you want to stop as its own process with the `-ports` flag:

```sh
# Terminal 1: Ctrl-C the existing mock, then start every port except 3004
go run ./cmd/mockupstream -ports 3001,3002,3003,3005,3006
# Terminal 5: the port you want to stop
go run ./cmd/mockupstream -ports 3004       # Ctrl-C to "take 3004 down"; rerun it to bring 3004 back
```
Do the same with 3001 (`/api/users`), 3002 (`/api/orders`), 3005 (`/api/legacy`) or 3006 (`/api/internal`).

### Stopping everything

Press Ctrl-C in the gateway terminal, then in each mock terminal. To make sure everything is down:
```sh
lsof -nP -iTCP -sTCP:LISTEN | grep -E ':(8080|300[1-6]) '    # should print nothing
```

---

## 2. Routes

### 2.1 `GET /health`

**Demonstrates:** the built-in health endpoint. It is handled before the router, needs no config and is always available.

**Success**
```sh
curl -s -i localhost:8080/health
```
```
HTTP/1.1 200 OK
Content-Type: application/json
X-Request-Id: 0dd263a6a4a7bd91

{"status":"healthy","uptime_seconds":4}
```

**Failure: wrong method → 405**
```sh
curl -s -i -X POST localhost:8080/health
```
```
HTTP/1.1 405 Method Not Allowed
Allow: GET
Content-Type: application/json

{"error":"method_not_allowed"}
```

**What to point out:** even `/health` gets an `X-Request-ID` and an access-log line (`"path":"/health","route":""`). _Talking point:_ "Health is wired in front of the router, so it works even if the route config is empty or broken."

---

### 2.2 `/api/users`: basic proxying + sliding-window rate limit

**Demonstrates:** a single `upstream.url`, methods `GET`/`POST`, `strip_prefix: false` (the full path is forwarded), and a per-route `sliding_window` limit of 30 per 60s per IP. It uses the global 30s timeout.

**Success: GET, with the full path forwarded**
```sh
curl -s -i localhost:8080/api/users/42
```
```
HTTP/1.1 200 OK
Content-Type: application/json
Server: mockupstream/1.0
X-Powered-By: mockupstream
X-Request-Id: 720c2cb338c888a4

{"upstream":"mock-3001","method":"GET","path":"/api/users/42","query":"","headers":{"Accept":["*/*"],"Accept-Encoding":["gzip"],"User-Agent":["curl/8.7.1"],"X-Forwarded-For":["::1"],"X-Forwarded-Host":["localhost:8080"],"X-Forwarded-Proto":["http"],"X-Request-Id":["720c2cb338c888a4"]}}
```
Look at the `X-Forwarded-*` headers, the forwarded `X-Request-Id`, and the unchanged path `/api/users/42`.

**Success: POST with a body**
```sh
curl -s -i -X POST localhost:8080/api/users -H 'Content-Type: application/json' -d '{"name":"ada"}'
```
```
HTTP/1.1 200 OK
...
{"upstream":"mock-3001","method":"POST","path":"/api/users",...,"body":{"name":"ada"}}
```

**Failure: method not allowed → 405 with an `Allow` header**
```sh
curl -s -i -X DELETE localhost:8080/api/users/42
```
```
HTTP/1.1 405 Method Not Allowed
Allow: GET, POST
Content-Type: application/json

{"error":"method_not_allowed","message":"DELETE is not allowed on /api/users"}
```

**Failure: path segment boundary → 404**
```sh
curl -s -i localhost:8080/api/usersX
```
```
HTTP/1.1 404 Not Found
Content-Type: application/json

{"error":"not_found","message":"no route matches /api/usersX"}
```

**Failure: rate limit → 429**
```sh
for i in $(seq 31); do curl -s -o /dev/null -w '%{http_code} ' localhost:8080/api/users; done; echo
curl -s -i localhost:8080/api/users
```
```
200 200 200 ... 200 429 429        (30 x 200 in the window counting earlier requests, then 429)
HTTP/1.1 429 Too Many Requests
Content-Type: application/json
Retry-After: 28

{"error":"rate_limited","message":"rate limit exceeded","retry_after":28}
```
In this run, two earlier requests had landed in the previous minute window, so only 28 of the loop's requests got through and `Retry-After` was 28s rather than about 60s. The sliding window weights the previous window's count by how much of that window still overlaps, so `Retry-After` depends on when your earlier requests fell.

**Waiting for the reset**
```sh
sleep 28; curl -s -o /dev/null -w '%{http_code}\n' localhost:8080/api/users      # → 200
```
Or read the wait time from the header (this sends one more request, which is rejected):
```sh
ra=$(curl -s -D - -o /dev/null localhost:8080/api/users | awk -F': ' 'tolower($1)=="retry-after"{print $2+0}'); echo "waiting ${ra:-0}s"; sleep ${ra:-0}
```
_(The `awk` one-liner was not run during verification.)_

**Failure: upstream down → 502** (stop port 3001 as described in [Stopping just one mock port](#stopping-just-one-mock-port))
```sh
curl -s -i localhost:8080/api/users/1
```
```
HTTP/1.1 502 Bad Gateway
Content-Type: application/json

{"error":"bad_gateway","message":"upstream request failed"}
```
Log line:
```json
{"level":"WARN","msg":"upstream request failed","route":"/api/users","path":"/api/users/1","status":502,"err":"dial tcp [::1]:3001: connect: connection refused"}
```

**What to point out:** the access-log line `{"msg":"request","id":...,"route":"/api/users","status":429,"duration_ms":0}`. A 429 never reaches the upstream. _Talking point:_ "Routing is a longest-prefix match on whole path segments, so `/api/usersX` doesn't leak into `/api/users`. The rate-limit check and the counter update happen under one mutex, so exactly N requests get through even under concurrency."

---

### 2.3 `/api/orders`: retries with exponential backoff, a 5s timeout and a fixed-window limit

**Demonstrates:** `retry` (3 attempts in total, exponential backoff of 1s then 2s, retried on 502/503/504), a route `timeout` override of 5s, and a `fixed_window` limit of 10 per 10s per IP. Methods: GET, POST, PUT.

**Success: GET and PUT**
```sh
curl -s -o /dev/null -w '%{http_code}\n' localhost:8080/api/orders                       # 200
curl -s -i -X PUT localhost:8080/api/orders/7 -H 'Content-Type: application/json' -d '{"qty":2}'
```
```
HTTP/1.1 200 OK
{"upstream":"mock-3002","method":"PUT","path":"/api/orders/7",...,"body":{"qty":2}}
```

**Success after retries: a flaky upstream**
```sh
for i in 1 2 3 4 5; do curl -s -o /dev/null -w '%{http_code} %{time_total}s\n' 'localhost:8080/api/orders?fail_rate=0.5'; done
```
```
200 0.001350s
200 3.005239s      ← failed twice, succeeded on the 3rd attempt
200 1.003639s      ← failed once, succeeded on the 2nd attempt
200 0.001405s
200 0.001308s
```
The results are random. Most runs end with all 200s, with a few 1s or 3s delays. Occasionally all three attempts fail, and you get a 503.

**Failure: retries exhausted → the upstream's last status (503) after about 3s**
```sh
curl -s -i -w '\ntime_total=%{time_total}s\n' 'localhost:8080/api/orders?status=503'
```
```
HTTP/1.1 503 Service Unavailable
Server: mockupstream/1.0
...
{"upstream":"mock-3002","method":"GET","path":"/api/orders","query":"status=503",...}
time_total=3.004784s
```
Logs (retry timing: +1s, then +2s):
```json
{"time":"19:32:42.034","msg":"retrying upstream request","route":"/api/orders","attempt":2,"of":3,"after_status":503,"delay":"1s"}
{"time":"19:32:43.036","msg":"retrying upstream request","route":"/api/orders","attempt":3,"of":3,"after_status":503,"delay":"2s"}
{"time":"19:32:45.037","msg":"request","id":"7f2e665a87233a6d","route":"/api/orders","status":503,"duration_ms":3003}
```

**Not retried: a status outside `on`**
```sh
curl -s -o /dev/null -w '%{http_code} %{time_total}s\n' 'localhost:8080/api/orders?status=500'     # 500 0.001s, no retry log lines
```

**The POST body is replayed on every attempt**
```sh
curl -s -i -X POST 'localhost:8080/api/orders?status=503' -H 'Content-Type: application/json' -d '{"item":"book"}'
```
```
HTTP/1.1 503 Service Unavailable
{"upstream":"mock-3002","method":"POST",...,"body":{"item":"book"}}      ← the 3rd attempt still had the body
```

**Failure: timeout → 504 after about 18s** (3 attempts × 5s, plus 1s and 2s of backoff)
```sh
curl -s -i -w '\ntime_total=%{time_total}s\n' 'localhost:8080/api/orders?delay=6s'
```
```
HTTP/1.1 504 Gateway Timeout
Content-Type: application/json

{"error":"gateway_timeout","message":"upstream did not respond in time"}
time_total=18.006659s
```
Logs:
```json
{"time":"19:33:28.119","msg":"retrying upstream request","route":"/api/orders","attempt":2,"of":3,"after_status":504,"delay":"1s"}
{"time":"19:33:34.122","msg":"retrying upstream request","route":"/api/orders","attempt":3,"of":3,"after_status":504,"delay":"2s"}
{"time":"19:33:41.124","level":"WARN","msg":"upstream request failed","route":"/api/orders","status":504,"err":"context deadline exceeded"}
{"time":"19:33:41.124","msg":"request","id":"ef7a9e66e33aa5d7","route":"/api/orders","status":504,"duration_ms":18005}
```

**Failure: upstream down → 502 after about 3s of retries** (stop port 3002)
```sh
curl -s -i -w '\ntime_total=%{time_total}s\n' localhost:8080/api/orders
```
```
HTTP/1.1 502 Bad Gateway
{"error":"bad_gateway","message":"upstream request failed"}
time_total=3.002520s
```
Logs: two `retrying upstream request ... "after_status":502` lines, then `"err":"dial tcp [::1]:3002: connect: connection refused"`.

**Recovery during a retry:** with 3002 down, send the request, then restart 3002 within about 1.5s:
```sh
curl -s -o /dev/null -w 'status=%{http_code} total=%{time_total}s\n' localhost:8080/api/orders
# meanwhile, in the 3002 terminal: go run ./cmd/mockupstream -ports 3002
```
```
status=200 total=3.007397s       ← attempts 1 and 2 got 502; attempt 3 reached the restarted upstream
```

**Failure: rate limit → 429 (10 per 10s, fixed window)**
```sh
for i in $(seq 11); do curl -s -o /dev/null -w '%{http_code} ' localhost:8080/api/orders; done; echo
curl -s -i localhost:8080/api/orders
```
```
200 200 200 200 200 200 200 200 200 200 429
HTTP/1.1 429 Too Many Requests
Retry-After: 5

{"error":"rate_limited","message":"rate limit exceeded","retry_after":5}
```
The windows are aligned to the wall clock (:00, :10, :20 and so on), so `Retry-After` is the time left until the next boundary, here 5s. Wait it out:
```sh
sleep 6; curl -s -o /dev/null -w '%{http_code}\n' localhost:8080/api/orders       # → 200
```

**Failure: 405 and 404**
```sh
curl -s -i -X DELETE localhost:8080/api/orders/7     # 405, Allow: GET, POST, PUT
curl -s -w ' %{http_code}\n' localhost:8080/api/ordersX   # {"error":"not_found",...} 404
```

**What to point out:** the `retrying upstream request` lines with `attempt`, `after_status` and `delay`, and the access log's `duration_ms` (3003 or 18005). _Talking point:_ "Retry sits above Dispatch, so every attempt gets its own 5s timeout and picks a target again. The worst case for this route is about 18s, and DECISIONS.md lists the lack of an overall deadline as a known limitation."

---

### 2.4 `/api/products`: `strip_prefix`, weighted load balancing and health checks

**Demonstrates:** `strip_prefix: true`, two `targets` with weights 3:1 and `weighted_round_robin` (smooth WRR), a 10s timeout, and an active `health_check` (`/healthz` every 30s, marked unhealthy after 3 failures). Method: GET only. There are no retries.

**Success: the prefix is stripped**
```sh
curl -s -i localhost:8080/api/products/123
curl -s 'localhost:8080/api/products/123?color=red' | jq -c '{upstream,path,query}'
curl -s localhost:8080/api/products | jq -c '{upstream,path}'
```
```
HTTP/1.1 200 OK
{"upstream":"mock-3003","method":"GET","path":"/123",...}
{"upstream":"mock-3004","path":"/123","query":"color=red"}
{"upstream":"mock-3003","path":"/"}
```

**Success: weighted 3:1 distribution**
```sh
for i in $(seq 8); do curl -s localhost:8080/api/products/1 | grep -o 'mock-300[0-9]'; done | tr '\n' ' '; echo
for i in $(seq 40); do curl -s localhost:8080/api/products/1 | grep -o 'mock-300[0-9]'; done | sort | uniq -c
```
```
mock-3003 mock-3003 mock-3003 mock-3004 mock-3003 mock-3003 mock-3003 mock-3004
  30 mock-3003
  10 mock-3004
```
Smooth WRR spreads the picks out rather than sending them in bursts. Where the cycle of 4 starts depends on earlier traffic. With jq, `jq -r .upstream` works in place of `grep -o`.

**Failure: 405 and 404**
```sh
curl -s -i -X POST localhost:8080/api/products/1     # 405, Allow: GET, "POST is not allowed on /api/products"
curl -s -o /dev/null -w '%{http_code}\n' localhost:8080/api/productsX     # 404
```

**Failure: timeout → 504 after 10s (no retry on this route)**
```sh
curl -s -i -w '\ntime_total=%{time_total}s\n' 'localhost:8080/api/products/1?delay=11s'
```
```
HTTP/1.1 504 Gateway Timeout
{"error":"gateway_timeout","message":"upstream did not respond in time"}
time_total=10.002120s
```

**Failure: one target down, then the health check removes it (about 60-90s with the example config)**

Run 3004 as its own process ([see above](#stopping-just-one-mock-port)) and stop it. Then run this loop and watch the events in Terminal 3:
```sh
for r in $(seq 22); do printf '%s ' "$(date +%T)"; for i in 1 2 3 4; do curl -s -o /dev/null -w '%{http_code} ' localhost:8080/api/products/1; done; echo; sleep 5; done
```
```
19:35:27 200 200 502 200        ← 3004 killed at 19:35:27; one request in four still goes to it
...
19:36:48 200 200 502 200
19:36:54 200 200 200 200        ← marked unhealthy; all traffic goes to 3003
```
```json
{"time":"19:36:51.882","level":"WARN","msg":"upstream target marked unhealthy","route":"/api/products","target":"http://localhost:3004","consecutive_failures":3}
```
The health checker probes once at startup and then every 30s. The target is marked down on the 3rd consecutive failed probe, so detection takes between 60s and 90s depending on when you stopped the target. In this run it took 84s. Until then, a quarter of the requests get 502, because this route has no retries.

Restart 3004. One successful probe brings it back, within 30s:
```json
{"time":"19:37:21.884","level":"INFO","msg":"upstream target is healthy again","route":"/api/products","target":"http://localhost:3004"}
```
```
19:37:24 mock-3003 mock-3003 mock-3004 mock-3003
```

**Faster version for a live demo:** use a copy of the config with a 2s interval.
```sh
sed 's/interval: "30s"/interval: "2s"/' gateway.yaml > /tmp/fast.yaml
go run ./cmd/gateway /tmp/fast.yaml 2> gateway.log
# stop 3004 and run the 4-request loop every second
```
```
0s: 200 200 502 200
...
6s: 200 200 502 200
7s: 200 200 200 200       ← "upstream target marked unhealthy" 6s after startup
```
**All targets down (fail open):** with the fast config, stop 3003 as well:
```json
{"level":"WARN","msg":"upstream target marked unhealthy","route":"/api/products","target":"http://localhost:3003","consecutive_failures":3}
{"level":"WARN","msg":"all upstream targets are unhealthy; failing open and sending traffic to all of them","route":"/api/products"}
```
Requests now return `502 502 502 502`: the gateway still tries the targets rather than returning a 503. After you restart both, they come back within one interval, and you'll see two `upstream target is healthy again` lines.

**What to point out:** the `marked unhealthy` and `healthy again` events. _Talking point:_ "Health checks only change which target the balancer picks. During the 60-90s detection gap, retries and the breaker are what protect clients, and that's why health checks were the lowest-priority tier."

---

### 2.5 `/api/legacy`: request and response transforms

**Demonstrates:** `strip_prefix`, request header add/remove (including `$request_time`), request body `mapping` (dot paths and `$literal:`), response header add/remove, and the response `envelope` (`$body`, `$response_time`, `$route_path`).

**Success: body mapping, header rules and the envelope**
```sh
curl -s -i -X POST localhost:8080/api/legacy/v1/data \
  -H 'Content-Type: application/json' -H 'X-Debug: 1' -H 'X-Internal: secret' \
  -d '{"userId": 42, "userName": "ada", "extra": true}'
```
```
HTTP/1.1 200 OK
Content-Type: application/json
X-Request-Id: 1b0fc26ee80b9f64
X-Served-By: gatewaykit                 ← added; Server and X-Powered-By are gone

{"data":{"body":{"meta":{"source":"gateway","timestamp":"2026-10-16T01:38:06.373Z"},"user":{"id":42,"name":"ada"}},
 "headers":{...,"X-Gateway":["gatewaykit"],"X-Request-Start":["2026-10-16T01:38:06.373Z"]},     ← no X-Debug or X-Internal
 "method":"POST","path":"/v1/data","query":"","upstream":"mock-3005"},
 "gateway_metadata":{"route":"/api/legacy","served_at":"2026-10-16T01:38:06.374Z"}}
```
To make it easier to read:
```sh
curl -s -X POST localhost:8080/api/legacy/v1/data -H 'Content-Type: application/json' -H 'X-Debug: 1' \
  -d '{"userId": 42, "userName": "ada", "extra": true}' | jq '{sent: .data.body, path: .data.path, meta: .gateway_metadata}'
```
Points to make: `extra` was dropped (the mapping outputs only the mapped fields), the upstream saw `/v1/data`, and the envelope keys are in alphabetical order.

**Success: GET with no body** (it is still enveloped, and no `body` key appears):
```sh
curl -s -i localhost:8080/api/legacy/v1/data
# HTTP/1.1 200 OK ... {"data":{"headers":{...},"method":"GET","path":"/v1/data",...,"upstream":"mock-3005"},"gateway_metadata":{...}}
```

**Success: a non-JSON body passes through unmapped**
```sh
curl -s -X POST localhost:8080/api/legacy/v1/data -H 'Content-Type: text/plain' -d 'hello world' -w '\n%{http_code}\n'
```
```
{"data":{"body":"hello world","headers":{...,"Content-Type":["text/plain"],...}},...}
200
```

**Success: a JSON object without a JSON Content-Type is still mapped** (`curl -d` defaults to a form type):
```sh
curl -s -X POST localhost:8080/api/legacy/v1/data -d '{"userId": 7}' | jq -c '{body: .data.body, ct: .data.headers["Content-Type"]}'
```
```
{"body":{"meta":{"source":"gateway","timestamp":"..."},"user":{"id":7}},"ct":["application/json"]}
```

**Failure: invalid JSON declared as `application/json` → 400** (the gateway's own error, so it is not enveloped and has no `X-Served-By`)
```sh
curl -s -i -X POST localhost:8080/api/legacy/v1/data -H 'Content-Type: application/json' -d '{"userId": 42,'
```
```
HTTP/1.1 400 Bad Request
Content-Type: application/json

{"error":"invalid_json","message":"request body is not valid JSON"}
```
The same broken bytes sent as `text/plain` are forwarded unchanged, with a 200 and `"body":"{\"userId\": 42,"`.

**Edge cases: valid JSON without the mapped fields** → 200, and only the literal and variable fields are sent:
```sh
curl -s -X POST localhost:8080/api/legacy/v1/data -H 'Content-Type: application/json' -d '{"somethingElse": 1}' | jq -c .data.body
# {"meta":{"source":"gateway","timestamp":"..."}}
curl -s -X POST localhost:8080/api/legacy/v1/data -H 'Content-Type: application/json' -d '[1,2]' | jq -c .data.body
# {"meta":{"source":"gateway","timestamp":"..."}}      ← a JSON array is accepted too (see Discrepancies)
```

**An upstream error is enveloped, and its status is kept**
```sh
curl -s -i 'localhost:8080/api/legacy/v1/data?status=404' | sed -n '1p;$p'
# HTTP/1.1 404 Not Found
# {"data":{...,"query":"status=404","upstream":"mock-3005"},"gateway_metadata":{"route":"/api/legacy",...}}
curl -s -i localhost:8080/api/legacy/healthz          # empty upstream body
# HTTP/1.1 200 OK ... {"data":null,"gateway_metadata":{"route":"/api/legacy","served_at":"..."}}
```

**Failure: 405**
```sh
curl -s -i -X DELETE localhost:8080/api/legacy/v1/data    # 405, Allow: GET, POST
```

**What to point out:** look at the response headers and the `data.headers` the upstream received, side by side. _Talking point:_ "Request transforms are `http.Handler` middleware and response transforms are `RoundTripper` decorators. Body runs before headers on the response, so the config's header rules have the final say. The gateway's own errors are never enveloped."

---

### 2.6 `/api/internal`: API key auth + circuit breaker

**Demonstrates:** `auth: api_key` (header `X-API-Key`, two keys) and a `circuit_breaker` (5 failures within a rolling 60s window trip it, with a 30s cooldown). It also uses the global default rate limit of 100 per 60s per IP.

**Success: either key, GET or POST**
```sh
curl -s -i -H 'X-API-Key: sk_live_abc123' localhost:8080/api/internal/stats
curl -s -H 'X-API-Key: sk_live_def456' -X POST localhost:8080/api/internal -d 'x' -w '\n%{http_code}\n'
```
```
HTTP/1.1 200 OK
{"upstream":"mock-3006","method":"GET","path":"/api/internal/stats",...,"headers":{...}}     ← X-API-Key is NOT forwarded upstream
{"upstream":"mock-3006","method":"POST",...,"body":"x"}
200
```
The header name is case-insensitive (`-H 'x-api-key: sk_live_abc123'` → 200). The key value is case-sensitive (`SK_LIVE_ABC123` → 401).

**Failure: missing key / wrong key → 401**
```sh
curl -s -i localhost:8080/api/internal
curl -s -i -H 'X-API-Key: wrong' localhost:8080/api/internal
```
```
HTTP/1.1 401 Unauthorized
{"error":"unauthorized","message":"missing X-API-Key header"}

HTTP/1.1 401 Unauthorized
{"error":"unauthorized","message":"invalid API key"}
```

**Failure: method → 405 (checked before auth)**
```sh
curl -s -i -X DELETE localhost:8080/api/internal     # 405 even without a key; Allow: GET, POST
```

**Failure: the circuit breaker trips → 503 with `retry_after`**
```sh
for i in $(seq 5); do curl -s -o /dev/null -w '%{http_code} ' -H 'X-API-Key: sk_live_abc123' 'localhost:8080/api/internal?status=500'; done; echo
curl -s -i -H 'X-API-Key: sk_live_abc123' localhost:8080/api/internal
```
```
500 500 500 500 500
HTTP/1.1 503 Service Unavailable
Content-Type: application/json
Retry-After: 30

{"error":"service_unavailable","retry_after":30}
```
```json
{"level":"WARN","msg":"circuit breaker state change","route":"/api/internal","from":"closed","to":"open"}
```
Five seconds later, `retry_after` has counted down (`{"error":"service_unavailable","retry_after":25}`). The upstream is not called: there is no `upstream request failed` line, and `duration_ms` is 0.

**Recovery (half-open probe).** After the 30s cooldown, the next request is the probe:
```sh
sleep 30; curl -s -o /dev/null -w '%{http_code}\n' -H 'X-API-Key: sk_live_abc123' localhost:8080/api/internal    # 200
```
```json
{"msg":"circuit breaker state change","route":"/api/internal","from":"open","to":"half-open"}
{"msg":"circuit breaker state change","route":"/api/internal","from":"half-open","to":"closed"}
```
If the probe fails, the breaker reopens for a full cooldown:
```sh
curl -s -i -H 'X-API-Key: sk_live_abc123' 'localhost:8080/api/internal?status=500' | head -1   # HTTP/1.1 500 (this is the probe)
curl -s -H 'X-API-Key: sk_live_abc123' localhost:8080/api/internal                            # {"error":"service_unavailable","retry_after":30}
```
```json
{"msg":"circuit breaker state change","from":"open","to":"half-open"}
{"msg":"circuit breaker state change","from":"half-open","to":"open"}
```

**A 4xx doesn't count as a failure**
```sh
for i in $(seq 6); do curl -s -o /dev/null -w '%{http_code} ' -H 'X-API-Key: sk_live_abc123' 'localhost:8080/api/internal?status=404'; done
curl -s -o /dev/null -w 'then %{http_code}\n' -H 'X-API-Key: sk_live_abc123' localhost:8080/api/internal
# 404 404 404 404 404 404 then 200
```

**Upstream down → 502 five times, then 503** (stop port 3006)
```sh
for i in $(seq 6); do curl -s -o /dev/null -w '%{http_code} ' -H 'X-API-Key: sk_live_abc123' localhost:8080/api/internal; done; echo
# 502 502 502 502 502 503
```
Restart 3006 and wait 30s. The next request is the probe, and it returns 200.

**Failure: global default rate limit → 429 (100 per 60s, fixed window aligned to the minute)**
```sh
for i in $(seq 105); do curl -s -o /dev/null -w '%{http_code}\n' -H 'X-API-Key: sk_live_abc123' localhost:8080/api/internal; done | sort | uniq -c
curl -s -o /dev/null -w 'no key while limited: %{http_code}\n' localhost:8080/api/internal
```
```
  99 200          ← plus 1 request earlier in the same minute = 100
   6 429
no key while limited: 401      ← auth runs before the rate limit
```

**What to point out:** the `circuit breaker state change` lines, and the 503 with `duration_ms: 0`. _Talking point:_ "The breaker sits above retry, so one client request counts as at most one failure, and an open breaker answers instantly without calling the upstream. Auth runs before rate limiting, so unauthenticated traffic can't use up a shared quota."

---

## 3. Gateway-wide behaviour

**Unknown path → 404**
```sh
curl -s -i localhost:8080/nope
```
```
HTTP/1.1 404 Not Found
Content-Type: application/json

{"error":"not_found","message":"no route matches /nope"}
```

**`/health` with POST → 405**: see [2.1](#21-get-health).

**Path cleaning before routing (and before auth)**
```sh
curl -s --path-as-is -w ' %{http_code}\n' localhost:8080/api/users/../internal
# {"error":"unauthorized","message":"missing X-API-Key header"} 401      ← routed as /api/internal, so auth applies
```

**Request ID round trip**
```sh
curl -s -i -H 'X-Request-ID: demo-123' localhost:8080/api/products/9 | grep -o -i -e '^x-request-id: .*' -e '"X-Request-Id":\[[^]]*\]'
grep demo-123 gateway.log
curl -s -D - -o /dev/null localhost:8080/nope | grep -i x-request-id
```
```
X-Request-Id: demo-123                         ← echoed to the client
"X-Request-Id":["demo-123"]                    ← forwarded to the upstream
{"msg":"request","id":"demo-123","method":"GET","path":"/api/products/9","route":"/api/products","status":200,...}
X-Request-Id: be7521b51a4b9a60                 ← generated when the client doesn't send one (on 404s too)
```

**Malformed config → fails at startup, exit code 1**
```sh
mkdir -p /tmp/gk
cat > /tmp/gk/bad-route.yaml <<'EOF'
gateway:
  port: 8080
routes:
  - path: "x"
    methods: ["FETCH"]
    upstream: {}
EOF
go run ./cmd/gateway /tmp/gk/bad-route.yaml; echo "exit=$?"
```
```
invalid config /tmp/gk/bad-route.yaml:
routes[0].path: must start with "/", got "x"
routes[0].methods[0]: unknown HTTP method "FETCH"
routes[0].upstream: one of url or targets is required
exit status 1
exit=1
```
(`exit status 1` is printed by `go run`. The built binary prints only the error lines.)

A typo in a key (unknown keys are rejected):
```sh
cat > /tmp/gk/bad-typo.yaml <<'EOF'
gateway:
  port: 8080
routes:
  - path: "/api/users"
    method: ["GET"]
    upstream:
      url: "http://localhost:3001"
EOF
go run ./cmd/gateway /tmp/gk/bad-typo.yaml
```
```
invalid config /tmp/gk/bad-typo.yaml:
parsing YAML: yaml: unmarshal errors:
  line 5: field method not found in type config.Route
```

Bad values, all reported at once (including an unknown `$variable`):
```sh
cat > /tmp/gk/bad-values.yaml <<'EOF'
gateway:
  port: 8080
routes:
  - path: "/api/orders"
    upstream:
      url: "localhost:3002"
    retry:
      attempts: 0
      backoff: "linear"
      on: [700]
    rate_limit:
      requests: 10
      window: "10s"
      strategy: "token_bucket"
      per: "user"
  - path: "/api/legacy"
    upstream:
      url: "http://localhost:3005"
    response_transform:
      body:
        envelope:
          data: "$bdy"
EOF
go run ./cmd/gateway /tmp/gk/bad-values.yaml
```
```
invalid config /tmp/gk/bad-values.yaml:
routes[0].upstream.url: must be an absolute http(s) URL, got "localhost:3002"
routes[0].retry.attempts: must be between 1 and 10, got 0
routes[0].retry.backoff: must be one of fixed, exponential, got "linear"
routes[0].retry.on[0]: not an HTTP status code: 700
routes[0].rate_limit.strategy: must be one of fixed_window, sliding_window, got "token_bucket"
routes[0].rate_limit.per: must be one of ip, global, got "user"
routes[1].response_transform.body.envelope.data: unknown variable "$bdy" (known: $body, $request_time, $response_time, $route_path, $literal:<value>)
```
A bad duration such as `global_timeout: "30 seconds"` gives `parsing YAML: line 3: invalid duration "30 seconds" (want e.g. "500ms", "30s", "1m")`. It stops at the parse step, so none of the errors above are reported. A missing file gives `reading config: open missing.yaml: no such file or directory`.

_Verification note:_ the `bad-typo` and `bad-values` configs were run with the built binary from a different directory, so the paths printed above were adjusted to match `/tmp/gk`.

### Hardening checks

These cover fixes made after a code review. Each was verified against commit `6efc6fd`.

**Ambiguous paths → 400.** A path that the gateway and an upstream could read differently is rejected before routing. The `;` case matters because a Java upstream (Tomcat, Spring) would strip `;x` and serve `/api/internal`, which would skip that route's auth.
```sh
curl -s -i 'localhost:8080/api/internal;x' | sed -n '1p;$p'
curl -s -i 'localhost:8080/api/products/a%2Fb' | sed -n '1p;$p'
```
```
HTTP/1.1 400 Bad Request
{"error":"bad_request","message":"ambiguous request path: contains \";\""}
HTTP/1.1 400 Bad Request
{"error":"bad_request","message":"ambiguous request path: contains an encoded slash or backslash"}
```

**A slow request body → 408 at the route timeout.** This promises 100 bytes, sends 1, then stalls. The 408 arrives after `/api/orders`' 5s timeout; `nc` itself exits when the `sleep` ends.
```sh
(printf 'POST /api/orders HTTP/1.1\r\nHost: x\r\nContent-Length: 100\r\n\r\n{'; sleep 8) | nc localhost 8080 | sed -n '1p;$p'
```
```
HTTP/1.1 408 Request Timeout
{"error":"request_timeout","message":"request body was not received in time"}
```

**Config values that would break at runtime are rejected at startup**
```sh
cat > /tmp/gk/bad-limits.yaml <<'EOF'
gateway:
  port: 8080
routes:
  - path: "/api/orders"
    upstream:
      url: "http://localhost:3002"
    retry:
      attempts: 1000
    request_transform:
      headers:
        add:
          "X Bad Name": "gatewaykit"
EOF
go run ./cmd/gateway /tmp/gk/bad-limits.yaml
```
```
invalid config /tmp/gk/bad-limits.yaml:
routes[0].retry.attempts: must be between 1 and 10, got 1000
routes[0].request_transform.headers.add.X Bad Name: invalid header name "X Bad Name"
```

**Three ways to pass the config path** (check the `"config"` field in the `gateway listening` line)
```sh
go run ./cmd/gateway -config gateway.yaml              # 1. flag
go run ./cmd/gateway /tmp/fast.yaml                    # 2. positional argument
GATEWAY_CONFIG=/tmp/fast.yaml go run ./cmd/gateway     # 3. environment variable
go run ./cmd/gateway                                   # 4. falls back to ./gateway.yaml
make run CONFIG=/tmp/fast.yaml                         #    with make
```
Precedence: `GATEWAY_CONFIG=nope.yaml ./bin/gateway -config gateway.yaml /tmp/gk/bad-route.yaml` starts with `"config":"gateway.yaml"`, so the flag wins. (Verified with the built binary; `make run CONFIG=` was not run.)

**Graceful shutdown during a slow request**
```sh
# Terminal 4
curl -s -o /dev/null -w 'slow request finished: %{http_code} after %{time_total}s\n' 'localhost:8080/api/users/1?delay=5s'
# within 5s, press Ctrl-C in the gateway terminal (the run below used kill -INT <pid>)
```
```
slow request finished: 200 after 5.004460s
```
```json
{"time":"19:41:13.949","msg":"shutting down; draining in-flight requests","grace":"15s"}
{"time":"19:41:17.950","msg":"request","id":"8b71bfce0b654f9a","path":"/api/users/1","status":200,"duration_ms":5003}
{"time":"19:41:18.228","msg":"gateway stopped"}
```
New connections are refused as soon as shutdown starts (`curl localhost:8080/health` → `000`, curl exit 7). A request longer than the 15s grace (`?delay=20s`) is cut off: curl gets an empty reply (exit 52) after about 16s, the gateway prints `shutdown: context deadline exceeded` and exits with 1, and no access-log line is written for that request.

---

## 4. Five-minute demo script

Start the mocks and the gateway, with Terminal 3 tailing the log. Each step is one or two commands.

```sh
# 1. Core: health, proxying and request ID (the echo shows what the upstream saw)
curl -s -i localhost:8080/health
curl -s -i -H 'X-Request-ID: demo-1' localhost:8080/api/users/42

# 2. Core: routing errors, with segment-boundary matching and Allow
curl -s -i localhost:8080/api/usersX
curl -s -i -X DELETE localhost:8080/api/users/42

# 3. Core: strip_prefix + weighted 3:1 load balancing
for i in $(seq 8); do curl -s localhost:8080/api/products/1 | grep -o 'mock-300[0-9]'; done | sort | uniq -c

# 4. Policy: API key auth
curl -s -i localhost:8080/api/internal
curl -s -i -H 'X-API-Key: sk_live_abc123' localhost:8080/api/internal

# 5. Policy: rate limit with Retry-After
for i in $(seq 11); do curl -s -o /dev/null -w '%{http_code} ' localhost:8080/api/orders; done; echo
# wait for the Retry-After from step 5 (at most 10s) before step 6, or it will be rate limited

# 6. Resilience: retries with exponential backoff (watch the log: +1s, +2s)
curl -s -o /dev/null -w '%{http_code} %{time_total}s\n' 'localhost:8080/api/orders?status=503'

# 7. Resilience: timeout → 504
curl -s -i -w 'time_total=%{time_total}s\n' 'localhost:8080/api/products/1?delay=11s'

# 8. Resilience: the circuit breaker trips, then fails fast
for i in $(seq 5); do curl -s -o /dev/null -w '%{http_code} ' -H 'X-API-Key: sk_live_abc123' 'localhost:8080/api/internal?status=500'; done; echo
curl -s -i -H 'X-API-Key: sk_live_abc123' localhost:8080/api/internal

# 9. Transforms: mapping, header rules, envelope
curl -s -i -X POST localhost:8080/api/legacy/v1/data -H 'Content-Type: application/json' -H 'X-Debug: 1' \
  -d '{"userId": 42, "userName": "ada", "extra": true}'

# 10. Transforms: invalid declared JSON is rejected before reaching the upstream
curl -s -i -X POST localhost:8080/api/legacy/v1/data -H 'Content-Type: application/json' -d '{"userId": 42,'

# 11. (Close) A bad config fails fast, and Ctrl-C drains in-flight requests
go run ./cmd/gateway /tmp/gk/bad-values.yaml
```
Talking order: "spec non-negotiables, then the policies the rubric names, then what happens when upstreams misbehave, then the transforms, which were the most ambiguous part and the last tier."

Each step was verified on its own in the sections above. The script was not run end to end in one sitting.

---

## 5. Discrepancies and surprises

1. **The fixed and sliding windows are aligned to the wall clock** (`now.Truncate(window)`). The README says "the 11th request inside 10s gets a 429". That holds only if the burst doesn't cross a :X0 boundary; one that does can get more than 10 200s. `Retry-After` is the time left until the boundary (5s in the run above, not 10s). The sliding window's `Retry-After` also depends on the previous aligned window (28s rather than about 60s).
2. **"Every problem is reported at once" applies only to semantic validation.** A YAML type error (a bad duration such as `"30 seconds"`) or an unknown key stops at the parse step, and none of the semantic errors are reported.
3. **Retry log lines have no request ID** (`retrying upstream request` carries only `route`), so with concurrent traffic you can't tie a retry to a request. Neither do `upstream request failed` or the breaker events.
4. **The error bodies are inconsistent.** The breaker's 503 (`{"error":"service_unavailable","retry_after":N}`) and `POST /health`'s 405 (`{"error":"method_not_allowed"}`) have no `message` field, unlike every other gateway error.
5. **405 is decided before auth.** An unauthenticated `DELETE /api/internal` gets 405 with `Allow: GET, POST` rather than 401, which reveals the route's methods to anyone.
6. **The body mapping accepts any valid JSON when it's declared as JSON.** `[1,2]` with `Content-Type: application/json` returns 200, and only the `$literal` and `$request_time` fields are sent. Missing source fields are dropped silently. This matches the code's comments but isn't in DECISIONS.md.
7. **Graceful shutdown has a hard 15s grace.** The README says Ctrl-C lets "in-flight requests finish first". A longer request is cut off: the client gets an empty reply, the gateway exits with 1 (`shutdown: context deadline exceeded`), and no access-log line is written.
8. **Health-check detection takes 60-90s, not "90s".** The first probe runs at startup and the rest every 30s, so marking a target down takes 2-3 intervals after the failure (84s in this run). The `/api/products` route has no retries, so a quarter of its requests return 502 during that gap.
9. **`make mock` is one process for all six ports.** You can't stop a single port without restarting with `-ports`, and the README doesn't mention the `-ports` flag.
10. **The API key is stripped before forwarding.** That's good, but it isn't documented. The header name is case-insensitive and the key is case-sensitive.
11. **Cosmetic:** the gateway adds `Accept-Encoding: gzip` to upstream requests (from Go's transport), and enveloped responses have no trailing newline.
12. **One unexplained timing:** the first `/api/orders?delay=6s` run took 23.98s in curl, while the gateway logged `duration_ms: 18006`, so about 6s passed before the gateway started handling it. Two reruns took 18.0s. It wasn't reproduced and may be an environment artifact.
