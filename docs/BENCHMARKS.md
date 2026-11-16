# Benchmarks

The numbers below are microbenchmarks of hot-path components, not end-to-end HTTP
throughput. They answer “where does CPU time go inside the gateway” and catch regressions
in allocation counts. End-to-end latency depends on the kernel, container network, TLS,
upstream and load generator; measure it on the host that will run the gateway using the
k6 profile described in section 5.

## 1. Environment

| Item | Value |
|---|---|
| Date | 2026-11-16 |
| OS | Windows 11, amd64 |
| CPU | 13th Gen Intel Core i5-13420H (12 logical threads) |
| Go | 1.27.1 |
| Command | `go test -run '^$' -bench . -benchmem ./internal/...` |
| Counters | default `-count=1`, `GOMAXPROCS=12` |

Power settings, background processes and thermal throttling move results by 5–15%. Treat
the numbers as relative: compare a change against this table on the same machine, not
against a different host.

## 2. Results

### Authentication

| Benchmark | ns/op | B/op | allocs/op | Approx ops/s |
|---|---:|---:|---:|---:|
| `AuthenticateJWT` (HS256, parse + verify + claims) | 12,247 | 2,851 | 56 | 81,700 |
| `AuthenticateAPIKey` (constant-time digest lookup) | 1,246 | 192 | 4 | 802,600 |
| `Authorize` (scope + role check) | 419 | 0 | 0 | 2,387,600 |

JWT verification is dominated by base64/JSON parsing and signature verification. API-key
authentication is an order of magnitude cheaper because it hashes one secret and does two
constant-time comparisons; this matches the design choice to prefer API keys for
high-volume machine clients. `Authorize` is allocation-free.

### Routing

| Benchmark | ns/op | B/op | allocs/op | Approx ops/s |
|---|---:|---:|---:|---:|
| `RouterMatch` (32 routes, exact host + prefix) | 1,188 | 32 | 1 | 841,800 |
| `RouterMatchMiss` (no route) | 226 | 64 | 2 | 4,429,000 |

The router is a compiled slice sorted by prefix length, not a trie. At 32 routes a hit
costs about 1.2 µs, which is negligible compared with any network hop. A miss exits early
and is cheaper. The single allocation is the route-match wrapper returned to the caller.

### Balancing and breaker

| Benchmark | ns/op | B/op | allocs/op | Approx ops/s |
|---|---:|---:|---:|---:|
| `AcquireRoundRobin` (16 targets) | 66.9 | 0 | 0 | 14,946,000 |
| `AcquireWeightedRoundRobin` (16 targets) | 212.9 | 0 | 0 | 4,697,000 |
| `AcquireLeastConn` (16 targets) | 921.0 | 0 | 0 | 1,085,800 |
| `BreakerAllowSuccess` | 173.4 | 56 | 3 | 5,767,000 |
| `BreakerAllowParallel` | 325.7 | 56 | 3 | 3,070,000 |

Round robin is an atomic increment plus modulo. Smooth weighted round robin holds a short
mutex to update current weights and is therefore slower but still sub-microsecond. Least
connections scans the 16 targets to find the minimum; cost grows linearly with pool size,
which is why the scan is bounded and pools are expected to stay small. The breaker
allocates the exactly-once completion closure on each admitted attempt; parallel
contention roughly doubles the cost.

### Rate limiting

| Benchmark | ns/op | B/op | allocs/op | Approx ops/s |
|---|---:|---:|---:|---:|
| `LocalAllow` (single hot key) | 108.8 | 0 | 0 | 9,191,000 |
| `LocalAllowManyKeys` (1,024 keys, sharded) | 121.1 | 0 | 0 | 8,258,000 |
| `BuildKey` (header key, SHA-256) | 1,430 | 96 | 6 | 699,300 |

The local fallback is allocation-free on the allow path. `BuildKey` is the expensive part
of header-based limiting because the header value is SHA-256 hashed to bound Redis key
cardinality; 1.4 µs is still small compared with a Redis round trip. The Redis path itself
is not timed here because its latency is dominated by the network and server, not Go.

### Configuration

| Benchmark | ns/op | B/op | allocs/op | Throughput |
|---|---:|---:|---:|---:|
| `ParseConfig` (10 upstreams, 50 routes) | 908,865 | 191,170 | 3,390 | 5.6 MB/s |
| `ParseConfig` + second `Validate` | 2,523,377 | 205,335 | 3,665 | — |

Config parsing is deliberately not a hot path: it runs at startup and on reload. The
defaults/validation pass is more expensive than YAML decoding because it checks every
route pair for ambiguity, every target URL, every limit and every breaker/retry invariant.
A sub-3 ms reload build is far below the debounce interval and is not a traffic concern.

## 3. Interpreting allocations

- **Router 1 alloc/op** — the returned match value escapes to the caller. Acceptable.
- **JWT 56 allocs/op** — standard library JWT parsing plus claim maps. Avoid JWT for
  high-QPS machine traffic if a static API key is possible.
- **Breaker 3 allocs/op** — the completion closure and its captured state. Required to
  guarantee exactly-once accounting across retries and errors.
- **Local limiter 0 allocs/op** — shards are preallocated and buckets are updated in
  place.
- **Config 3.4k allocs/op** — irrelevant to request latency; it runs off the hot path.

Run with `-benchmem` and compare `allocs/op` first. A change that doubles allocations on
the request path is more likely to hurt p99 than a small `ns/op` change.

## 4. Profiling a regression

Microbenchmarks locate the component; pprof locates the line:

```bash
go test -run '^$' -bench BenchmarkRouterMatch -benchmem \
  -cpuprofile cpu.out -memprofile mem.out ./internal/router
go tool pprof -http=:8081 cpu.out
```

For a running gateway, the admin listener exposes pprof behind the admin token:

```bash
curl -o cpu.out -H "Authorization: Bearer $GATEWAY_ADMIN_TOKEN" \
  "http://127.0.0.1:9090/debug/pprof/profile?seconds=30"
go tool pprof -http=:8081 cpu.out
```

Always capture a goroutine profile when latency rises without CPU:

```bash
curl -H "Authorization: Bearer $GATEWAY_ADMIN_TOKEN" \
  "http://127.0.0.1:9090/debug/pprof/goroutine?debug=2" > goroutines.txt
```

## 5. End-to-end load profile

The k6 script mixes the anonymous public route with the API-key-authenticated users route,
so routing, auth, rate limiting, balancing and proxying are all exercised:

```bash
cp deploy/.env.example deploy/.env
docker compose -f deploy/docker-compose.yml --profile load run --rm \
  -e K6_VUS=50 -e K6_DURATION=60s k6
```

Useful knobs:

| Variable | Default | Meaning |
|---|---|---|
| `BASE_URL` | `http://gateway:8080` in compose | target |
| `K6_VUS` | 50 | concurrent virtual users |
| `K6_DURATION` | 30s | scenario duration |
| `API_KEY` | demo key | API key for the authenticated route |

The script fails its thresholds if:

- `http_req_failed` rate ≥ 1%;
- p95 latency ≥ 250 ms or p99 ≥ 500 ms;
- fewer than 99% of checks pass.

Run a ramp to find the saturation knee: 10, 25, 50, 100, 200 VUs for 60 s each. Record
request rate, p50/p95/p99, 5xx ratio, retry rate, goroutines and CPU. Stop increasing when
p99 crosses the target or the 5xx rate rises; that is the capacity of the host, not a
property of the gateway software.

Compare against the mock upstream directly (`http://localhost:9001` inside the compose
network) to separate gateway overhead from backend time:

```bash
docker compose -f deploy/docker-compose.yml run --rm --entrypoint sh k6 -c \
  'wget -qO- http://mock-a:9001/healthz'
```

For a full fault matrix, run the same profile under the chaos overlay and compare
[CHAOS.md](CHAOS.md) scenarios. A baseline without the corresponding fault run is not a
useful benchmark.

## 6. Recording results

When changing the hot path, include in the pull request:

1. the exact command and hardware;
2. before/after rows for every affected benchmark;
3. allocation changes, with an explanation for any increase;
4. whether `go test -race ./...` and the coverage gate still pass.

Do not present microbenchmark ops/s as gateway throughput. The network, TLS, body sizes
and upstream latency dominate real deployments; the k6 profile is the source of truth for
end-to-end capacity.
