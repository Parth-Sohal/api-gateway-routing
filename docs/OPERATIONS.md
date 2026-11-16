# Operations guide

This guide covers deployment, probes, reload, shutdown, observability and troubleshooting.
Configuration fields are documented in [CONFIGURATION.md](CONFIGURATION.md); internal
behavior is described in [ARCHITECTURE.md](ARCHITECTURE.md).

## 1. Deployment shapes

### Single binary

```bash
make build                       # bin/gateway
GATEWAY_ADMIN_TOKEN=... \
GATEWAY_JWT_SECRET=... \
./bin/gateway -config configs/gateway.yaml
```

The process runs in the foreground, logs to stdout, and handles SIGINT/SIGTERM. Secrets
must come from the environment or mounted files, never the YAML.

### Docker Compose

```bash
cp deploy/.env.example deploy/.env    # replace the development secrets
docker compose -f deploy/docker-compose.yml up --build -d
```

Services: one gateway, Redis, three mock upstreams, Prometheus and Grafana. The `load`
profile adds k6; the chaos overlay adds Toxiproxy.

```bash
# Follow logs.
docker compose -f deploy/docker-compose.yml logs -f gateway

# Stop and remove volumes.
docker compose -f deploy/docker-compose.yml down -v
```

### Image

`Dockerfile` is a multi-stage build. The runtime image contains `/app/gateway`,
`/app/mockupstream`, `/app/token` and the configs. It runs as a non-root user, exposes
8080/9090/9100 and defaults to `-config /app/configs/gateway.compose.yaml`.

```bash
docker build --build-arg VERSION=$(git describe --tags --always) -t api-gateway-routing:dev .
```

### Ports and exposure

| Port | Purpose | Exposure |
|---|---|---|
| 8080 | public traffic | clients / ingress |
| 9090 | admin API, dashboard, pprof, probes | private network only |
| 9100 | Prometheus metrics | private network only |
| 6379 | Redis | private network only |
| 9091 / 3000 | Prometheus / Grafana (demo) | local or private |
| 8474 | Toxiproxy API (chaos only) | local only |

Never publish the admin or metrics listener to the internet. The admin token protects the
API, but the dashboard and probe paths are unauthenticated by design.

## 2. Startup and probes

Startup order:

1. parse and validate the config (fatal on error);
2. bind the public listener;
3. start admin and metrics listeners (if enabled);
4. connect to Redis when rate limits exist; strict limits fail startup if Redis is down;
5. build the first state and start active health checks;
6. mark readiness true.

| Endpoint | Purpose | Expected |
|---|---|---|
| `GET :9090/healthz` | liveness | 200 while the process is alive |
| `GET :9090/readyz` | readiness | 200 when serving; 503 while starting, shutting down, or when strict Redis is unavailable |
| `GET :9090/dashboard` | embedded UI | 200 |

Kubernetes-style probes:

```yaml
livenessProbe:
  httpGet: { path: /healthz, port: 9090 }
  periodSeconds: 10
readinessProbe:
  httpGet: { path: /readyz, port: 9090 }
  periodSeconds: 5
  failureThreshold: 2
```

Readiness drops immediately on shutdown, before in-flight requests finish. This removes
the pod from service endpoints before the drain deadline.

## 3. Configuration reload

Reload is supported from a file change or the admin API:

```bash
# Validate first; this does not touch the running process.
go run ./cmd/gateway -config /etc/gateway/gateway.yaml -validate

# Apply through the admin listener.
curl -X POST -H "Authorization: Bearer $GATEWAY_ADMIN_TOKEN" \
  http://127.0.0.1:9090/admin/reload
```

A reload parses, validates and builds a complete new snapshot before swapping. On failure
the old snapshot keeps serving; the error is logged, counted in
`gateway_config_reloads_total{result="failure"}` and shown in
`GET /admin/api/overview`.

Fixed at startup and **not** hot-reloadable: public/admin/metrics listen addresses, the
Redis client and limiter mode. Changing those requires a rolling restart. Routes, auth,
rate-limit rules, upstream targets, health checks, breakers and retries are reloadable.

fsnotify watches the config file's parent directory (250 ms debounce), which supports
atomic replace and Kubernetes ConfigMap symlink swaps. The watcher keeps running after a
failed reload.

Reload checklist:

1. Change the config in version control.
2. Run `-validate` against the new file.
3. Apply with a rolling restart or `/admin/reload`.
4. Check `gateway_config_reloads_total{result="success"}` increments.
5. Check `GET /admin/api/upstreams` for unexpected health changes.
6. Roll back by restoring the previous file; the next reload applies it.

## 4. Graceful shutdown

On SIGTERM/SIGINT:

1. readiness flips to false (`"shutting down"`);
2. listeners stop accepting new connections;
3. in-flight requests drain for `server.shutdown_grace` (default 15s);
4. active health checks are cancelled; idle upstream sockets close;
5. the process exits 0.

If the drain deadline expires, the server force-closes and exits non-zero. Keep the
orchestrator termination grace period slightly larger than `shutdown_grace`.

`server.write_timeout` defaults to 0 to keep streaming responses working. If you set a
finite write timeout for non-streaming traffic, ensure it is compatible with your longest
allowed response.

## 5. Admin API

All endpoints except probes/dashboard require `Authorization: Bearer <admin token>`.

```bash
TOKEN=$GATEWAY_ADMIN_TOKEN

curl -s -H "Authorization: Bearer $TOKEN" http://127.0.0.1:9090/admin/api/overview
curl -s -H "Authorization: Bearer $TOKEN" http://127.0.0.1:9090/admin/api/upstreams
curl -s -H "Authorization: Bearer $TOKEN" http://127.0.0.1:9090/admin/api/config
curl -s -X POST -H "Authorization: Bearer $TOKEN" http://127.0.0.1:9090/admin/reload
```

- **overview** — version, uptime, ready state, route/upstream counts, limiter mode,
  reload count, last reload time/error.
- **upstreams** — per-target health, inflight count and breaker state.
- **config** — the effective config rendered as redacted YAML; secrets are removed.
- **reload** — validate and apply the watched file.

pprof is on the same listener under `/debug/pprof/*` and requires the token:

```bash
go tool pprof -http=:8081 \
  "http://127.0.0.1:9090/debug/pprof/profile?seconds=30" \
  -header "Authorization: Bearer $TOKEN"
```

## 6. Metrics, dashboards and alerts

Prometheus scrapes `:9100/metrics` every 5 s (`deploy/prometheus/prometheus.yml`). The
provisioned Grafana dashboard is **API Gateway Overview** (`deploy/grafana/dashboards/`).

Panels cover request rate, 5xx ratio, p99 latency, inflight requests, responses by status,
latency percentiles, upstream outcomes, target health, breaker state, rate-limit decisions,
auth failures and retries.

Alert rules live in `deploy/prometheus/alerts.yml`:

| Alert | Condition | Severity |
|---|---|---|
| `GatewayDown` | scrape `up == 0` for 1m | critical |
| `GatewayHighErrorRate` | 5xx ratio > 5% for 5m | critical |
| `GatewayHighLatencyP99` | p99 > 1s for 5m | warning |
| `GatewayRateLimitedSpike` | > 25 limited req/s for 5m | warning |
| `GatewayAuthFailureSpike` | > 10 auth failures/s for 5m | warning |
| `GatewayTargetUnhealthy` | a target unhealthy for 2m | warning |
| `GatewayBreakerOpen` | breaker state open for 1m | critical |
| `GatewayAllTargetsDown` | every target in an upstream unhealthy | critical |
| `GatewayUpstreamServerErrors` | upstream 5xx ratio > 10% | warning |
| `GatewayConfigReloadFailed` | any failed reload in 10m | warning |
| `GatewayRetryRateHigh` | retries > 20% of attempts | warning |

Useful queries:

```promql
# Request rate by route.
sum by (route) (rate(gateway_http_requests_total[5m]))

# 5xx ratio.
sum(rate(gateway_http_requests_total{status=~"5.."}[5m]))
  / sum(rate(gateway_http_requests_total[5m]))

# p99 latency.
histogram_quantile(0.99,
  sum by (le) (rate(gateway_http_request_duration_seconds_bucket[5m])))

# Rate-limit pressure.
sum by (route, decision) (rate(gateway_ratelimit_decisions_total[5m]))

# Breakers currently open.
gateway_breaker_state == 2
```

## 7. Logs

Structured logs go to stdout. Important events:

| Event | Level | Fields |
|---|---|---|
| startup | info | version, addresses, limiter mode |
| config loaded/reloaded | info | path, routes, upstreams, duration |
| reload failed | error | error, path; old snapshot remains |
| target ejected/recovered | warn/info | upstream, target, reason |
| breaker transition | warn/info | upstream, from, to, reason |
| rate limiter unavailable | warn | route, error |
| proxy failure | warn/error | upstream, target, reason, request_id |
| shutdown | info | grace, drained |

Access logs contain request ID, route, method, path, status, bytes, duration, client IP,
principal log ID and error code. Authorization headers, API keys, cookies and bodies are
never logged.

Do not log secrets in upstream configuration. The admin config endpoint redacts secret
fields, but raw YAML on disk must still be protected.

## 8. Capacity tuning

| Symptom | Knob |
|---|---|
| CPU saturation from many connections | `server.max_concurrent_requests`, transport pool sizes |
| upstream connection churn | `max_idle_conns_per_host`, `idle_conn_timeout` |
| retry storms | lower `retry.budget_percent`, `max_attempts`; widen breaker |
| slow upstream eats gateway capacity | lower `route.timeout`, `response_header_timeout` |
| Redis latency spikes | raise `redis.op_timeout` modestly; use fail-open for fairness limits |
| 429 bursts too aggressive | raise `capacity`/`refill_per_sec`; verify key type |
| too many goroutines on reload | reduce upstream count/targets; inspect pprof |

Rules of thumb:

- Keep `max_idle_conns_per_host` at or above expected concurrency per target.
- Keep `retry.max_attempts` small (2–3) and the budget at 10% unless upstreams are known
  to be flaky.
- Set `route.timeout` below the upstream's own deadline, leaving room for retries.
- Use `fail_open: false` only for security-critical limits; it makes Redis a hard
  dependency.
- Bound label cardinality by keeping route names stable. Do not generate route names per
  tenant if you can match by host instead.

## 9. Troubleshooting

| Symptom | Likely cause | Action |
|---|---|---|
| 404 on a known path | prefix boundary or host mismatch | check `match.path_prefix` and `match.host`; remember `/api/users` does not match `/api/users-archive` |
| 405 | path matched, method not allowed | inspect the `Allow` header and `match.methods` |
| 401 with valid token | wrong issuer/audience/secret/expiry | decode the JWT; check `iss`, `aud`, `exp`, algorithm and leeway |
| 403 with valid token | missing scope/role | compare token claims with `auth.scopes`/`auth.roles`; remember `match` semantics |
| 429 immediately | tiny bucket or shared IP/header key | inspect limit headers; verify `key` and `header` |
| all requests 503 `upstream_unavailable` | breaker tripped | check upstream errors, `open_timeout`, breaker state; fix upstream then wait for half-open |
| 503 `limiter_unavailable` | strict limit + Redis down | restore Redis or switch to fail-open; readiness should be false |
| 502 | connection/DNS/TLS failure | check target URL reachability and logs |
| 504 | timeout waiting for headers | increase route/transport timeout or fix upstream latency |
| 413 | body exceeds `max_body_bytes` | raise the route limit or stream without retry |
| reload rejected | strict YAML/validation error | run `-validate`; the old config is still active |
| readiness flapping | strict Redis dependency or draining | check `/readyz` body reason and Redis health |
| high memory | buffered retry bodies or many idle connections | lower `body_buffer_bytes`, pool sizes, concurrency |

Capture a profile before changing capacity knobs:

```bash
curl -H "Authorization: Bearer $TOKEN" \
  "http://127.0.0.1:9090/debug/pprof/goroutine?debug=2" > goroutines.txt
```

## 10. Security checklist

- Replace every development secret in `deploy/.env`; never commit `.env`.
- Keep the admin and metrics listeners on a private interface.
- Run as a non-root user (the image does this by default).
- Mount configs read-only; restrict who can write them, since a reload executes no code
  but changes routing and auth policy.
- Store API keys as SHA-256 hashes; rotate by adding a new ID, then removing the old one.
- Set `trusted_proxies` only to your actual load balancer CIDRs.
- Use RS256 with a mounted public key when tokens are issued by an external IdP.
- Keep `max_body_bytes`, `max_header_bytes` and timeouts finite.
- Scrape metrics privately; do not expose raw paths or credentials as labels.
- Review `gateway_auth_failures_total` and `gateway_ratelimit_decisions_total` for abuse.
