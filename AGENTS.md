# AGENTS.md — build and contribution guide

Companion to the API gateway design document. The design document is the **what/why**;
this file is the **how/in what order**. Read both before changing code.

## Mission

A production-shaped, self-hosted API gateway in Go: routing, load balancing, auth,
distributed rate limiting, circuit breaking, retries, metrics, hot reload, admin API and
dashboard. Correctness, observability and testability matter more than feature count.

## Golden rules

1. **No package-level mutable globals.** Dependencies are passed explicitly; `cmd` only wires.
2. **Immutable config snapshots.** Requests load one snapshot and use it for their lifetime.
   Reload builds a new state and atomically swaps it.
3. **Bounded everything.** Timeouts, body sizes, pools, queues, label cardinality, retry budget.
4. **Every goroutine takes a context and exits on cancellation.** No leaked tickers/watchers.
5. **Interfaces at the seams.** `Limiter`, `Authenticator`, `Balancer`, `Clock` are small and fakeable.
6. **Never hold a lock across I/O.** Redis/network calls happen outside mutexes.
7. **Errors are typed and mapped once.** Gateway responses use `internal/httpx`; no leaking internals.
8. **Secrets only from env or mounted files.** Never log credentials or request bodies.
9. **`go test -race ./...` is mandatory.** Coverage ≥80% on config, auth, ratelimit,
   breaker, balancer, router, proxy.
10. **Document tradeoffs in docs, not just code comments.**

## Environment

- Go 1.26+ (module declares `go 1.26.0`).
- Redis 6.2+ for integration tests and compose.
- Docker 24+ for compose/chaos/load.
- Windows or Linux for development; CI runs Linux.

## Repository layout

```
cmd/gateway           main wiring, flags, lifecycle
cmd/mockupstream      demo/benchmark upstream with fault endpoints
cmd/token             demo JWT minting + API-key hash generation
cmd/loadgen           small Go load generator (fallback when k6 unavailable)
internal/config       YAML parsing, defaults, validation, fsnotify watcher, redaction
internal/httpx        error envelope, request info/ID, client IP, path normalization, recorder
internal/router       route table and matching (longest prefix, host, method)
internal/auth         JWT/API-key authentication, principal, authorization
internal/ratelimit    Redis+Lua, local sharded fallback, fail-open policy
internal/breaker      per-upstream circuit breaker and rolling window
internal/balancer     round robin, smooth weighted RR, least connections
internal/health       active probes + passive health registry
internal/proxy        forwarding, retries, retry budget, streaming, error mapping
internal/metrics      Prometheus registry and helpers
internal/observability slog setup
internal/middleware   recovery, request ID, access log/metrics
internal/admin        admin API, pprof, embedded dashboard
internal/server       state building, listeners, hot reload, graceful shutdown
deploy/               Dockerfile, compose, Prometheus, Grafana, Toxiproxy
bench/k6              load profiles
docs/                 architecture, config, ops, testing, chaos, benchmarks
```

Dependency direction: `cmd → server → {middleware, router, proxy, admin, config}`;
`proxy → {balancer, breaker, health, metrics, httpx}`. Leaf packages must never import
`middleware` or `server`. No import cycles.

## Build order (each step compiles, is tested, and is committed separately)

1. **Scaffold** — module, docs, Makefile, config examples, CI skeleton.
2. **`internal/clock` + `internal/httpx`** — fakeable clock, ULID request IDs, client IP
   extraction with trusted proxies, error envelope, response recorder, path normalization.
3. **`internal/config`** — strict YAML, defaults, aggregate validation, redaction, watcher.
4. **`internal/router`** — matching + tie detection; unit tests for precedence/405.
5. **`internal/auth`** — JWT HS256/RS256, API keys (constant-time), scopes/roles; tests for
   alg confusion, expiry, revocation.
6. **`internal/ratelimit`** — Lua token bucket, Redis limiter, sharded local limiter,
   fallback wrapper; atomicity test (1,000 goroutines → exactly capacity).
7. **`internal/breaker`** — state machine, rolling window, half-open probes; fake-clock tests.
8. **`internal/balancer` + `internal/health`** — strategies, hysteresis, active checker.
9. **`internal/proxy`** — forwarding, header hygiene, retries/budget, streaming, breaker
   recording, error mapping; httptest tests.
10. **`internal/metrics` + `observability` + `middleware`** — catalog, access logs, recovery.
11. **`internal/admin` + dashboard** — protected admin API, pprof, embedded UI.
12. **`internal/server`** — immutable state, hot reload, listeners, readiness, graceful drain.
13. **`cmd/*`** — gateway wiring, mock upstream, token tool, loadgen.
14. **Integration/E2E tests** — full stack, multi-instance limits, failover, reload, shutdown.
15. **Deploy** — Dockerfile, compose, Prometheus alerts, Grafana, Toxiproxy, k6.
16. **Docs and measurement** — fill benchmark/chaos numbers with real results; CI.

## Commands

```bash
make fmt            # gofmt -w .
make vet            # go vet ./...
make test           # go test ./...
make test-race      # go test -race ./...
make cover          # coverage report per core package
make cover-gate     # enforce the >=80% floor on mandated packages
make test-integration
make bench          # go test -bench . -benchmem ./...
make load           # k6 via compose
make chaos
make docker
```

On Windows without `make`, run the underlying `go` commands directly.

## Testing conventions

- Prefer table-driven tests and `t.Parallel()` where safe.
- Inject `clock.Clock`; never `time.Sleep` to test state machines.
- Use `httptest` for upstreams and `miniredis` for Redis unit tests.
- Integration tests that need a real Redis skip with `t.Skip` unless
  `GATEWAY_TEST_REDIS` is set (CI sets it).
- Use `goleak` in lifecycle tests; close servers, clients and watchers.
- Every bug fix gets a regression test.

## Definition of done for a phase

- Code compiles; `go vet` and `gofmt` clean.
- New behavior has tests, including at least one failure-path test.
- No unbounded goroutines, maps or label values introduced.
- Metrics/log fields documented if operator-visible.
- README/docs updated when config or behavior changes.
- Logical conventional commit (`feat(ratelimit): ...`) pushed to `main`.

## Operational invariants to preserve

- Rate-limit Lua script is O(1) and uses Redis `TIME`; never move token math client-side.
- Breaker `done` callback is called exactly once per attempt, including on retry and error.
- `Retry-After` is set on 429/503 from limit/breaker; headers are never overwritten by upstream.
- Config reload failure leaves the old snapshot serving traffic.
- Readiness drops before shutdown; in-flight requests are drained before exit.
- `/admin/*` requires the admin token; pprof is never exposed on the public listener.
- Logs contain request IDs but never `Authorization`, API keys, cookies or bodies.
