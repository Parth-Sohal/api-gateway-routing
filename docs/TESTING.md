# Testing guide

The project is tested at three levels: fast package tests, full-stack integration tests,
and load/chaos experiments. The first two run on every push; the third is manual and
documented in [CHAOS.md](CHAOS.md) and [BENCHMARKS.md](BENCHMARKS.md).

The golden rule is that failure paths are tested as deliberately as success paths. A
change that only adds a happy-path assertion is not finished.

## 1. Test pyramid

| Layer | Location | What it proves | Runtime |
|---|---|---|---|
| Unit | `internal/<pkg>/*_test.go` | matching, validation, state machines, header hygiene, error mapping | seconds |
| Component | package tests with `httptest`, `miniredis`, fake clock | proxy/limiter/breaker behavior under controlled failures | seconds |
| Integration | `internal/server/integration_test.go` | real listeners, router, auth, proxy, admin, watcher, Redis | tens of seconds |
| Lifecycle | `internal/server/integration_lifecycle_test.go` | graceful drain, goleak, multi-instance limits | tens of seconds |
| Load/chaos | `bench/k6`, `deploy/docker-compose.chaos.yml` | throughput, latency, failover under injected faults | manual |

Unit tests never bind fixed ports and never depend on wall-clock sleeps for state
transitions. `internal/clock` supplies a fake clock to the breaker, limiter and health
tests. Network tests use `httptest.Server`, Redis unit tests use `miniredis`, and the
one real-Redis integration test is opt-in.

## 2. Commands

```bash
make test             # go test ./...
make test-race        # go test -race ./...
make cover            # coverage profile over ./internal/...
make cover-gate       # enforce >=80% on the seven mandated packages
make test-integration # -race, only Integration|MultiInstance|Graceful tests
make bench            # benchmarks with -benchmem
make fuzz             # 20s each for config, httpx and router fuzz targets
```

On Windows without `make`, run the underlying commands:

```powershell
go test ./...
go test -race ./...
go test -coverprofile coverage.out ./internal/...
go tool cover -func coverage.out
go test -run '^$' -bench . -benchmem ./internal/...
```

PowerShell can interpret `-coverprofile=coverage.out` as a parameter assignment and
truncate the filename. Prefer `-coverprofile coverage.out` when running directly.

Race detection needs cgo and a C toolchain. On a Windows host without gcc, run it in
WSL:

```bash
wsl -d Ubuntu -- bash -lc \
  "cd /mnt/d/Go_Final_1 && CGO_ENABLED=1 ~/goroot/go/bin/go test -race ./... -count=1"
```

CI always runs `go test -race -count=1 ./...` on Linux with Redis available.

## 3. Coverage gate

`AGENTS.md` mandates at least 80% statement coverage for:

```
internal/config
internal/auth
internal/ratelimit
internal/breaker
internal/balancer
internal/router
internal/proxy
```

`scripts/coverage-gate.sh` runs each package separately, parses the coverage line and
fails if any package is below `COVERAGE_THRESHOLD` (default 80). Separate runs keep the
number attributable to a package instead of diluted by a whole-repo profile.

```bash
bash scripts/coverage-gate.sh
COVERAGE_THRESHOLD=85 bash scripts/coverage-gate.sh   # stricter local check
```

Coverage is a floor, not a goal. The important assertions are the failure paths listed in
section 6.

## 4. Real Redis

Most limiter tests use `miniredis`, which is fast and deterministic but does not exercise
the real Lua runtime. The multi-instance test is skipped unless `GATEWAY_TEST_REDIS` is
set:

```bash
# Linux/macOS or WSL
GATEWAY_TEST_REDIS=127.0.0.1:6379 go test -count=1 \
  ./internal/server -run TestIntegrationMultiInstanceRateLimitRealRedis
```

```powershell
$env:GATEWAY_TEST_REDIS = "127.0.0.1:6379"
go test -count=1 ./internal/server -run TestIntegrationMultiInstanceRateLimitRealRedis
```

The test starts two gateway instances against one Redis, sends more than the bucket
capacity and asserts the combined admitted count never exceeds capacity. It is the test
that proves token math is server-side and atomic across processes.

CI starts `redis:7.4-alpine` as a service and sets `GATEWAY_TEST_REDIS=127.0.0.1:6379`.

## 5. What each package test covers

| Package | Representative tests |
|---|---|
| `config` | strict YAML (`KnownFields`), defaults, aggregate validation, route ambiguity, secret redaction, fsnotify reload and failed reload retention |
| `httpx` | error envelope, ULID request IDs, trusted-proxy client IP, malformed `X-Forwarded-For`, path normalization, response recorder |
| `router` | longest prefix, boundary (`/api` vs `/api-x`), host wildcard, method 405 with `Allow`, ambiguity rejection |
| `auth` | HS256/RS256, alg confusion, expiry/issuer/audience, API-key constant-time lookup, revoked/expired keys, scope/role `any`/`all`, identity headers |
| `ratelimit` | Lua token bucket atomicity, limit/reset/retry values, sharded local fallback, fail-open/fail-closed wrapper, key cardinality |
| `breaker` | failure threshold, ratio window, open/half-open/closed transitions, probe cap, exactly-once completion, stale-completion isolation |
| `balancer` | round robin, smooth weighted RR distribution, least connections, exclusion set, unhealthy skipping |
| `health` | active probe thresholds, hysteresis, passive records, target identity across reloads |
| `proxy` | forwarding and hop-by-hop removal, retry eligibility/backoff/budget, body buffering and 413, streaming after headers, error mapping, breaker/health accounting |
| `server` | full stack, failover, reload/retention, shutdown drain, distributed limits, goleak |
| `admin` | token auth, probes, overview/upstreams/config, redaction, reload, pprof protection |
| `middleware` | recovery envelope, access log fields, request ID propagation |
| `metrics` | registry isolation, helper label sets, handler exposure |

## 6. Failure-path checklist

Every feature should have at least one test for each applicable row:

| Scenario | Expected |
|---|---|
| malformed config / unknown field | startup/reload error; old snapshot retained |
| Redis unavailable, `fail_open: true` | request allowed, `failopen` decision recorded |
| Redis unavailable, `fail_open: false` | 503 `limiter_unavailable`, readiness false |
| invalid/expired JWT, wrong algorithm | 401, no internal detail in body |
| missing scope/role | 403 |
| invalid credentials on optional route | 401, not anonymous |
| route method mismatch | 405 with `Allow` |
| path traversal / encoded dot segments | rejected before routing |
| body over limit | 413 without contacting upstream |
| retry budget exhausted | original response returned, no retry |
| upstream connection error | 502 or retry to a different target |
| upstream timeout | 504 |
| breaker open | 503 `circuit_open` with `Retry-After` |
| all targets unhealthy | 503 `service_unavailable` |
| client cancellation | upstream attempt cancelled; no retry |
| panic in handler | 500 envelope, access log still emitted |
| shutdown with in-flight request | readiness false first, request completes, exit clean |
| failed reload | old routes still serving, reload failure counted |

## 7. Fuzzing

Three fuzz targets cover the parser and request-matching boundary:

```bash
go test -run '^$' -fuzz FuzzParse  -fuzztime 30s ./internal/config
go test -run '^$' -fuzz FuzzClientIP -fuzztime 30s ./internal/httpx
go test -run '^$' -fuzz FuzzMatch -fuzztime 30s ./internal/router
```

`FuzzParse` asserts arbitrary YAML never panics and any accepted config is valid.
`FuzzClientIP` asserts malformed peer/forwarded chains never panic.
`FuzzMatch` asserts arbitrary host/path/method input never panics and a match always
returns a route.

Corpus entries that find a bug become regression seeds in `testdata/fuzz/` and are
committed with the fix.

## 8. Goroutine hygiene

`internal/server` uses `goleak.VerifyTestMain`, so any leaked watcher, health checker,
listener or local-limiter janitor fails the package. Integration helpers start servers
with a context and always call shutdown plus the cleanup function. If a test hangs, run
it with `-timeout 60s` and capture goroutine stacks before changing the leak settings.

## 9. Race and parallel tests

- `t.Parallel()` is used where tests do not share mutable state or environment.
- Limiter atomicity is tested with concurrent goroutines; the Redis script test asserts
  exactly capacity admissions from 1,000 concurrent requests.
- Breaker completion is called from concurrent attempts; exactly-once behavior is tested.
- Balancer selection is exercised concurrently to catch lost updates in counters.

Run the whole suite at least once with `-race` before pushing. A green non-race run is not
sufficient for concurrency-sensitive changes.

## 10. Integration test topology

`internal/server` integration tests start real servers on ephemeral loopback ports:

- a public listener with the full middleware chain;
- an admin listener with a test token;
- an in-process or miniredis Redis;
- `httptest` upstreams that can fail, delay, stream or count attempts.

The tests exercise the same `server.New` path as production, so a wiring error cannot be
hidden by constructing handlers manually. They assert both status/body and observable
side effects (admin overview, metrics values, attempt counts, breaker state).

## 11. CI pipeline

`.github/workflows/ci.yml` runs on pushes and pull requests to `main`:

1. gofmt check (`gofmt -l .` must be empty);
2. `go vet ./...`;
3. `go mod tidy` and fail if `go.mod`/`go.sum` change;
4. `go build ./...`;
5. `go test -race -count=1 ./...` with Redis;
6. coverage gate.

The workflow cancels superseded runs on the same ref and has a 20-minute timeout.

## 12. Adding a test

1. Put the test beside the code (`package foo`, not `foo_test`, unless testing the public
   surface).
2. Use table-driven cases with explicit names.
3. Inject the fake clock for anything time-dependent.
4. Assert the error type/code, not only the message.
5. Add the failure case that motivated the change.
6. Run `gofmt`, `go vet`, the package test with `-race`, then the full suite.
7. Keep new fixtures small; do not commit captured production traffic or secrets.
