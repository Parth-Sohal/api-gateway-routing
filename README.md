# API Gateway

A config-driven, self-hosted reverse-proxy API gateway written in Go. One binary provides
routing, load balancing, JWT/API-key authentication, distributed rate limiting, circuit
breaking, retries, Prometheus metrics, structured logs, hot config reload, an admin API and
a small operations dashboard.

```
                        ┌──────────────────────────────────────────────┐
                        │               gateway process                │
 clients ── HTTP ─────► │ :8080  middleware → router → auth → limit    │
                        │        → breaker → balancer → proxy ─────────┼──► mock-a
                        │ :9090  admin API + dashboard + pprof         │──► mock-b
                        │ :9100  /metrics                              │──► mock-c
                        └───────────────┬──────────────────────────────┘
                                        │ token buckets (atomic Lua)
                                     ┌──▼───┐
                                     │Redis │
                                     └──────┘
```

## Features

- **Routing** by longest path prefix, host (including `*.example.com`), and HTTP method.
- **Reverse proxy** built on `net/http` with per-upstream connection pools, streaming
  responses, hop-by-hop header handling, body limits and layered timeouts.
- **Load balancing**: round robin, smooth weighted round robin, least connections.
- **Health checks**: active probes plus passive ejection with failure/success hysteresis.
- **Authentication**: JWT (`HS256`, `RS256`) and hashed API keys; per-route scopes/roles.
- **Distributed rate limiting**: Redis token bucket implemented in a single atomic Lua
  script; sharded in-memory fallback when Redis is unavailable (`fail_open` policy).
- **Circuit breaker** per upstream: rolling failure window, ratio/consecutive trip rules,
  half-open probes, `Retry-After` on rejection.
- **Retries** for idempotent requests with exponential backoff, jitter, target exclusion
  and a global retry budget.
- **Hot reload** via fsnotify or `POST /admin/reload`; immutable config snapshots; invalid
  reloads keep the previous state.
- **Observability**: Prometheus metrics, JSON access logs, request IDs, pprof on the admin
  port, provisioned Grafana dashboard and alert rules.
- **Operations**: admin API, embedded dashboard, `/healthz`, `/readyz`, graceful shutdown
  with connection draining.
- **Zero external Go services**: Redis is required only for distributed limit accuracy.

Requirements: Go 1.26+, Redis 6.2+ for distributed limits, Docker 24+ for the demo stack.

## Quick start (Docker Compose)

```bash
# Optional: copy the example environment and replace the development secrets.
cp deploy/.env.example deploy/.env

docker compose -f deploy/docker-compose.yml up --build -d
docker compose -f deploy/docker-compose.yml ps
```

This starts one gateway, Redis, three mock upstreams, Prometheus and Grafana. The optional
`load` profile runs the k6 baseline against the stack.

| Service | URL |
|---|---|
| Public gateway | http://localhost:8080 |
| Admin API + dashboard | http://localhost:9090/dashboard |
| Metrics | http://localhost:9100/metrics |
| Prometheus | http://localhost:9091 |
| Grafana | http://localhost:3000 |
| Toxiproxy API (chaos overlay) | http://localhost:8474 |

Development credentials used by the compose defaults:

- API key: `gw_demo_demo-secret-123` (stored only as a SHA-256 hash in the config)
- Admin token: `development-admin-token`
- JWT secret: `development-only-secret`
- Grafana login: `admin` / `development-grafana-password`

Mint a JWT inside the container and call the protected routes:

```bash
# JWT for the users/orders routes.
docker compose -f deploy/docker-compose.yml exec gateway \
  sh -c '/app/token -secret "$GATEWAY_JWT_SECRET" -sub user-42 \
         -scopes "users:read,orders:read"'

# Public route: no authentication.
curl -i http://localhost:8080/public/hello

# Protected route using the demo API key (users:read).
curl -i -H "X-API-Key: gw_demo_demo-secret-123" \
  http://localhost:8080/api/users/me

# Protected route using a JWT.
curl -i -H "Authorization: Bearer <token>" \
  http://localhost:8080/api/orders

# Admin API and dashboard.
curl -H "Authorization: Bearer development-admin-token" \
  http://localhost:9090/admin/api/overview
```

Check metrics, readiness and hot reload:

```bash
curl http://localhost:9090/readyz
curl http://localhost:9100/metrics | head
curl -X POST -H "Authorization: Bearer development-admin-token" \
  http://localhost:9090/admin/reload
```

## Local development

```bash
# 1. Optional Redis for distributed limits.
docker run -d --name gw-redis -p 6379:6379 redis:7-alpine

# 2. Three mock upstreams.
go run ./cmd/mockupstream -addr 127.0.0.1:9001 -name mock-1 &
go run ./cmd/mockupstream -addr 127.0.0.1:9002 -name mock-2 &
go run ./cmd/mockupstream -addr 127.0.0.1:9003 -name mock-3 &

# 3. Development secrets (PowerShell: $env:NAME = "value").
export GATEWAY_ADMIN_TOKEN=development-admin-token
export GATEWAY_JWT_SECRET=development-only-secret

# 4. Run the gateway.
go run ./cmd/gateway -config configs/gateway.yaml

# 5. Mint a token and call the gateway.
go run ./cmd/token -secret "$GATEWAY_JWT_SECRET" -sub user-42 \
  -scopes "users:read,orders:read"
```

Useful flags:

```bash
go run ./cmd/gateway -config configs/gateway.yaml -validate   # validate and exit
go run ./cmd/gateway -config configs/gateway.yaml -log-level debug
go run ./cmd/gateway -version
```

## Routes in the sample config

| Route | Match | Upstream | Auth | Limit |
|---|---|---|---|---|
| `users` | `/api/users*` | users (3 targets) | JWT or API key, `users:read` | 100 burst / 50 rps per IP |
| `orders` | `/api/orders*` | orders (weighted) | JWT, `orders:read` | none |
| `public` | `/public*` | users | none | none |

Both `/api` routes strip `/api` before forwarding. The mock upstream exposes `/healthz`,
any echo path, and `/debug/status?code=500`, `/debug/slow?ms=2000`,
`/debug/flaky?rate=0.3` for failure injection.

## Configuration

See [docs/CONFIGURATION.md](docs/CONFIGURATION.md) for the full reference and
[`configs/gateway.yaml`](configs/gateway.yaml) for a working example.

Secrets are never written in YAML. The config references environment variables or mounted
files (`*_env`, `*_file`). API keys are stored as SHA-256 hashes.

```yaml
routes:
  - name: users
    match:
      path_prefix: /api/users
      methods: [GET, PUT]
    upstream: users
    strip_prefix: /api
    auth:
      required: true
      methods: [jwt, api_key]
      scopes: [users:read]
      match: any
    rate_limit:
      capacity: 100
      refill_per_sec: 50
      key: ip
      fail_open: true
    timeout: 30s
    max_body_bytes: 1048576
```

Four configurations are included:

| File | Purpose |
|---|---|
| `configs/gateway.yaml` | local development with `127.0.0.1` upstreams |
| `configs/gateway.compose.yaml` | compose network with Redis and mock containers |
| `configs/gateway.toxiproxy.yaml` | chaos overlay, targets routed through Toxiproxy |
| `deploy/prometheus/*`, `deploy/grafana/*` | scrape config, alerts and dashboard |

## Architecture and documentation

- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) — components, request lifecycle, concurrency,
  failure policies and design tradeoffs.
- [docs/CONFIGURATION.md](docs/CONFIGURATION.md) — every configuration field and default.
- [docs/OPERATIONS.md](docs/OPERATIONS.md) — deployment, reload, shutdown, dashboards,
  alerts and troubleshooting.
- [docs/TESTING.md](docs/TESTING.md) — test pyramid, integration setup, coverage gates.
- [docs/BENCHMARKS.md](docs/BENCHMARKS.md) — reproducible benchmark methodology and results.
- [docs/CHAOS.md](docs/CHAOS.md) — fault-injection playbook with Toxiproxy.
- [AGENTS.md](AGENTS.md) — build order and engineering conventions for contributors/agents.

## Endpoints

| Port | Purpose |
|---|---|
| `:8080` | public traffic |
| `:9090` | admin API, dashboard, pprof, health/readiness |
| `:9100` | Prometheus `/metrics` |

Gateway-generated errors use a stable envelope:

```json
{"error":{"code":"rate_limited","message":"Too many requests. Retry after 3 seconds.","request_id":"01J..."}}
```

Admin endpoints:

| Endpoint | Authentication | Purpose |
|---|---|---|
| `GET /healthz` | none | liveness |
| `GET /readyz` | none | readiness (Redis dependency when limits are strict) |
| `GET /dashboard` | none | embedded operations dashboard |
| `GET /admin/api/overview` | bearer token | build, uptime, reload and traffic summary |
| `GET /admin/api/upstreams` | bearer token | per-target health, inflight and breaker state |
| `GET /admin/api/config` | bearer token | effective config with secrets redacted |
| `POST /admin/reload` | bearer token | validate and atomically apply the config file |
| `/debug/pprof/*` | bearer token | CPU, heap and goroutine profiles |

## Testing

```bash
make test                    # unit tests
make test-race               # race detector
make cover-gate              # >=80% coverage on the mandated core packages
make test-integration        # full-stack tests (set GATEWAY_TEST_REDIS for real Redis)
make bench                   # Go microbenchmarks
make chaos                   # Toxiproxy overlay (compose)
make load                    # k6 baseline (compose profile)
```

Quality gates enforced by CI: `gofmt`, `go vet`, `go test -race ./...` with a Redis service,
`go mod tidy` cleanliness and the per-package coverage floor. Integration tests that need a
real Redis skip unless `GATEWAY_TEST_REDIS=host:port` is set.

## Known limitations (v1)

- Single Redis is a single point of failure for limit *accuracy*; availability is preserved
  by the local fallback (`fail_open: true`).
- Circuit-breaker state is per gateway instance (no Redis round trip on the hot path).
- Static upstream targets only; no service discovery.
- HTTP/1.1 and HTTP/2 only; no WebSocket, gRPC or HTTP/3 proxying.
- No response caching or request transformation.

See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for the failure model and tradeoffs.
