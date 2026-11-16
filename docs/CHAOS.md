# Chaos and load experiments

The chaos overlay runs the same gateway and mock upstreams, but every upstream target
points at a Toxiproxy listener instead of the mock container directly. That allows
latency, outages and partial failures to be injected while the process keeps running and
Prometheus keeps scraping.

Read [OPERATIONS.md](OPERATIONS.md) for the ordinary deployment and
[TESTING.md](TESTING.md) for the deterministic test suite. This document is about
experiments that produce real latency and error numbers.

## 1. Topology

```text
gateway :8080 ──► toxiproxy :8666 ──► mock-a :9001
        :9090              :8667 ──► mock-b :9001
        :9100              :8668 ──► mock-c :9001

Prometheus :9091    Grafana :3000    Toxiproxy API :8474
```

Toxiproxy listeners are defined in `deploy/toxiproxy/toxiproxy.json`:

| Proxy | Listener | Upstream |
|---|---|---|
| `mock-a` | `8666` | `mock-a:9001` |
| `mock-b` | `8667` | `mock-b:9001` |
| `mock-c` | `8668` | `mock-c:9001` |

The gateway config is `configs/gateway.toxiproxy.yaml`:

- `users` uses `least_conn` over all three proxies with active health checks, a breaker
  and three retry attempts;
- `orders` uses smooth weighted round robin over `mock-a` (weight 2) and `mock-b`
  (weight 1);
- the public route has no auth; `users` requires `users:read`; `orders` requires
  `orders:read`.

## 2. Start and smoke test

```bash
cp deploy/.env.example deploy/.env
docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.chaos.yml up --build -d
```

```bash
curl -i http://localhost:8080/public/echo
curl -i -H "X-API-Key: gw_demo_demo-secret-123" http://localhost:8080/api/users/echo
curl -s http://localhost:8474/proxies | jq
curl -s -H "Authorization: Bearer $GATEWAY_ADMIN_TOKEN" http://localhost:9090/admin/api/upstreams | jq
```

Tear down with the same two files:

```bash
docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.chaos.yml down -v
```

The mock upstream also exposes direct fault endpoints. Through the public route they are
reachable at `/public/debug/...`:

| Endpoint | Behavior |
|---|---|
| `GET /debug/status?code=503` | return an arbitrary status 200–599 |
| `GET /debug/slow?ms=2000` | sleep up to 60 s or cancel with the request context |
| `GET /debug/flaky?rate=1` | return 503 with probability `rate` (0–1) |
| `GET /healthz` | liveness used by active health checks |

## 3. Experiment A — baseline

Load without faults to establish the reference:

```bash
docker compose -f deploy/docker-compose.yml --profile load run --rm \
  -e K6_VUS=50 -e K6_DURATION=60s k6
```

Record from Grafana or Prometheus:

- request rate and p50/p95/p99 latency;
- 5xx ratio;
- upstream attempts and retry rate;
- CPU and goroutines (`gateway_http_inflight_requests`, process metrics).

The k6 thresholds fail the run if `http_req_failed ≥ 1%`, p95 ≥ 250 ms or p99 ≥ 500 ms.
Adjust `K6_VUS` upward until latency or error thresholds break; that point is the
saturation knee for the host.

## 4. Experiment B — latency on one target

Add 300 ms latency and jitter to `mock-a`:

```bash
curl -s -X POST http://localhost:8474/proxies/mock-a/toxics \
  -H 'Content-Type: application/json' \
  -d '{"name":"latency","type":"latency","attributes":{"latency":300,"jitter":50}}'
```

Run the load profile again. Expected:

- `least_conn` moves most new work to `mock-b`/`mock-c`;
- p99 rises but stays below the k6 threshold while two targets are healthy;
- `gateway_upstream_duration_seconds{target="http://toxiproxy:8666"}` shows the extra
  300 ms;
- no breaker transition, because the request still succeeds within the route timeout.

Raise latency above the route timeout (30 s for `users`) to force timeouts instead:

```bash
curl -s -X POST http://localhost:8474/proxies/mock-a/toxics/latency \
  -H 'Content-Type: application/json' \
  -d '{"name":"latency","type":"latency","attributes":{"latency":31000,"jitter":0}}'
```

Expected: attempts to `mock-a` hit the route deadline, map to 504, consume retry budget
and eventually count as breaker failures. Other targets keep serving, so overall
availability should remain intact until the retry budget or breaker blocks the upstream.

Remove the toxic:

```bash
curl -s -X DELETE http://localhost:8474/proxies/mock-a/toxics/latency
```

## 5. Experiment C — one target outage

Disable the proxy so connections to `mock-a` are refused:

```bash
curl -s -X POST http://localhost:8474/proxies/mock-a -d '{"enabled":false}'
```

Expected sequence:

1. in-flight and new attempts to `mock-a` get connection errors → 502 or retry;
2. passive health records failures; active probes fail every 2 s with an
   `unhealthy_threshold` of 3, so the target is ejected after roughly 6 s;
3. `gateway_target_healthy{target="http://toxiproxy:8666"}` drops to 0;
4. `least_conn` stops selecting it; the retry attempt goes to another target;
5. overall 5xx rate rises briefly at the start, then returns to baseline.

Re-enable it and watch the `healthy_threshold: 2` hysteresis:

```bash
curl -s -X POST http://localhost:8474/proxies/mock-a -d '{"enabled":true}'
```

The target returns only after two consecutive successful probes, not on the first
success. This prevents flapping.

## 6. Experiment D — all targets down

```bash
for p in mock-a mock-b mock-c; do
  curl -s -X POST "http://localhost:8474/proxies/$p" -d '{"enabled":false}'
done
```

Expected:

- all targets unhealthy → requests fail with `503 service_unavailable`;
- retries stop being useful and are bounded by the per-request attempt count and the
  global retry budget;
- `gateway_target_healthy == 0` for every target, and the
  `GatewayAllTargetsDown` alert fires after 2 minutes;
- the gateway stays alive, serves readiness (Redis is still up), and recovers without a
  restart when the proxies are re-enabled.

This experiment distinguishes “upstream pool exhausted” (503) from “breaker open”
(503 with `circuit_open`) and “gateway overloaded” (503 with `overloaded`). Always inspect
the error `code` in the response body, not only the status.

## 7. Experiment E — 5xx storm and breaker

Make the mock return 503 on every public request:

```bash
for i in $(seq 1 30); do
  curl -s -o /dev/null "http://localhost:8080/public/debug/flaky?rate=1"
done
```

Expected:

- each request may be retried up to `max_attempts: 3`;
- 503 responses count as breaker failures (`count_500` is not required for 5xx other than
  500);
- once the threshold/ratio trips, `gateway_breaker_state{upstream="users"}` becomes 2
  (open) and requests fail fast with `503 circuit_open` plus `Retry-After`;
- after `open_timeout: 5s`, up to three half-open probes are admitted; failures reopen
  the breaker, successes close it after two consecutive successes.

Watch transitions:

```bash
watch -n1 'curl -s -H "Authorization: Bearer $GATEWAY_ADMIN_TOKEN" \
  http://localhost:9090/admin/api/upstreams | jq ".upstreams[] | {name, breaker}"'
```

Stop sending failures and call the healthy echo endpoint until the breaker closes. A
breaker is per upstream and per instance, so with multiple gateway replicas the state can
differ briefly; this is an accepted tradeoff documented in
[ARCHITECTURE.md](ARCHITECTURE.md).

## 8. Experiment F — Redis outage and limiter fallback

With the default fail-open policy:

```bash
docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.chaos.yml stop redis
```

Expected:

- `users` keeps serving through the sharded local limiter;
- `gateway_ratelimit_decisions_total{decision="failopen"}` increases;
- `gateway_ratelimit_decisions_total{decision="error"}` increases while Redis is down;
- readiness stays true because no route is strict;
- after `docker compose ... start redis`, decisions return to `allowed`/`limited`.

If a route has `fail_open: false`, the same experiment must produce
`503 limiter_unavailable` and `/readyz` must return 503 with `redis unavailable`. Do this
only in a separate stack: the limiter mode and Redis client are fixed at startup, so
changing the policy requires a restart.

## 9. Experiment G — config reload under load

While the k6 profile runs, replace the config with an invalid document:

```bash
cp configs/gateway.toxiproxy.yaml /tmp/gateway.backup.yaml
printf 'routes: [\n' > configs/gateway.toxiproxy.yaml
```

The file watcher notices the malformed file and rejects it. Expected:

- traffic continues on the old snapshot;
- `gateway_config_reloads_total{result="failure"}` increments;
- `/admin/api/overview` reports the last reload error;
- restoring the valid file triggers a successful reload.

Then add a new route or change a rate limit and observe the new behavior without a
restart. Remember that listener addresses and the Redis client are not reloadable.

## 10. Experiment H — graceful shutdown

Run a slow request and stop the gateway:

```bash
curl -s "http://localhost:8080/public/debug/slow?ms=3000" &
sleep 0.5
docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.chaos.yml stop gateway
```

Expected:

- `/readyz` would return 503 immediately (before the listener drains);
- the in-flight request completes if it fits within `shutdown_grace` (15 s);
- the container exits 0; no goroutine leak is reported by the lifecycle tests.

This is the container-level version of
`TestIntegrationGracefulShutdownDrainsInflight`.

## 11. Experiment I — retry budget under partial failure

Set a high failure rate on all mocks by sending `rate=0.5` to `/public/debug/flaky` and
drive load with k6:

```bash
K6_VUS=100 K6_DURATION=2m docker compose -f deploy/docker-compose.yml \
  -f deploy/docker-compose.chaos.yml --profile load run --rm k6
```

Expected:

- retries rise but stay bounded by `retry.budget_percent: 10` in steady state;
- `gateway_upstream_retries_total{reason=...}` shows the reasons;
- p99 grows with backoff, but the gateway does not amplify traffic without bound;
- if the budget is exhausted, the original failure is returned instead of another
  attempt.

Compare with `budget_percent: 50` in a branch/config copy to see the amplification
tradeoff. Do not ship the higher value without measuring upstream capacity.

## 12. Metrics to record

After each experiment, capture at least:

```promql
sum by (route) (rate(gateway_http_requests_total[1m]))
sum by (route, status) (rate(gateway_http_requests_total[1m]))
histogram_quantile(0.99, sum by (le) (rate(gateway_http_request_duration_seconds_bucket[1m])))
sum by (upstream, target, outcome) (rate(gateway_upstream_requests_total[1m]))
sum by (upstream) (rate(gateway_upstream_retries_total[1m]))
gateway_target_healthy
gateway_breaker_state
sum by (route, decision) (rate(gateway_ratelimit_decisions_total[1m]))
sum(rate(gateway_http_requests_total{status=~"5.."}[1m]))
  / sum(rate(gateway_http_requests_total[1m]))
```

Store the query output or a Grafana snapshot next to the experiment notes. Without the
numbers, a chaos run is only an anecdote.

## 13. Safety notes

- Run chaos only against the local compose stack, never a shared environment.
- Toxiproxy removal or disable affects every gateway replica pointed at that proxy.
- Do not run the load profile against production upstreams; the mock endpoints inject
  failures by design.
- The admin token in `.env.example` is a development value. Replace it before exposing
  anything beyond loopback.
- Reset every toxic and re-enable every proxy after an experiment. A forgotten outage
  looks like a real incident to the next person.
