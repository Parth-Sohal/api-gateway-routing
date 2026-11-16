# Architecture

This document describes the runtime shape of the gateway, the order in which policy
is applied, and the tradeoffs behind the failure policies. The configuration field
reference lives in [CONFIGURATION.md](CONFIGURATION.md); operational procedures live in
[OPERATIONS.md](OPERATIONS.md).

## 1. Goals and non-goals

The gateway is a **self-hosted edge for HTTP APIs**. It owns:

- matching requests to routes and forwarding them to static upstream pools;
- authenticating callers and enforcing per-route scopes/roles;
- applying distributed token-bucket limits;
- protecting unhealthy upstreams with a circuit breaker and health ejection;
- retrying idempotent requests within a global budget;
- exposing metrics, logs, admin state and graceful lifecycle.

It deliberately does **not** provide service discovery, response caching, request
transformation, WebSocket/gRPC proxying, or a control plane. Those are separate concerns;
keeping them out makes the hot path small and the failure model auditable.

## 2. Process, listeners and ownership

`cmd/gateway` only parses flags, loads the config, creates the logger and metrics
registry, and calls `server.New` / `Server.Run`. All request logic lives under
`internal/`.

| Listener | Default | Owner | Exposure |
|---|---|---|---|
| Public | `:8080` | `server` | client traffic |
| Admin | `127.0.0.1:9090` | `admin.Handler` | private network only |
| Metrics | `127.0.0.1:9100` | `metrics.Handler` | Prometheus scrape |

The admin listener carries `/healthz`, `/readyz`, the dashboard, the JSON admin API,
`/admin/reload` and `/debug/pprof/*`. The metrics listener serves only the Prometheus
registry. Admin and metrics must never be published to the public internet; compose maps
them to host ports only for the local demo.

There are no package-level mutable globals. A `Metrics` value, logger, clock, limiter and
config snapshot are passed explicitly through constructors. The `cmd` packages perform
wiring only.

## 3. Immutable serving state and hot reload

A `state` is one complete serving snapshot:

```text
validated *config.Config
router.Router          compiled route table
auth.Authenticator     JWT/API-key verifiers
proxy.Proxy            per-upstream transport, balancer, breaker, health, retry budget
ratelimit.Limiter      Redis or local fallback (fixed at startup)
http.Handler           RequestID -> AccessLog -> Recovery -> serve
```

`state` is immutable after construction. Each request loads the current pointer once and
uses that snapshot for its entire lifetime. A reload:

1. reads and strictly parses the YAML file (`KnownFields(true)`; unknown fields fail);
2. applies defaults and validates every cross-field invariant;
3. builds a new `state` (router, authenticator, proxy runtimes, health checks);
4. atomically swaps the pointer;
5. closes the old state, which cancels its health-check goroutines and closes idle
   upstream connections.

If any step fails, the old snapshot keeps serving and the error is recorded in metrics,
logs and the admin overview. In-flight requests continue on the old state until they
finish. Listeners and the Redis/local limiter wiring are startup-fixed; changing their
addresses or the rate-limit backend requires a process restart.

Reloads are triggered by fsnotify (watching the parent directory so ConfigMap symlink
swaps are detected; 250 ms debounce) or by `POST /admin/reload` with the admin token.

## 4. Request lifecycle

```text
RequestID -> AccessLog -> Recovery
  -> concurrency semaphore (optional)
  -> path normalization
  -> router match (404/405)
  -> authenticate (401) -> authorize (403)
  -> rate limit (429)
  -> proxy.Forward
       -> request body preparation / size limit (413)
       -> per-route timeout
       -> retry loop:
            balancer acquire -> breaker allow -> upstream attempt
            -> health record -> breaker completion -> retry decision
       -> final response headers/status/body streaming
```

Middleware order is intentional:

- **RequestID** first: every log line and error envelope has an ID, even for panics.
- **AccessLog** next: the recorder observes the final status and byte count from all
  later layers, including recovery and overload responses.
- **Recovery** innermost: a panic is converted to a 500 envelope and a metric; the
  access log still records it.

The concurrency semaphore rejects with `503 overloaded` and a short `Retry-After` instead
of queueing without bound. The limit is `server.max_concurrent_requests`; zero disables it.

Path normalization rejects NUL/control characters, malformed percent-encoding,
backslashes, dot segments and duplicate slashes. The normalized path, not the raw path, is
used for routing and forwarding, which prevents encoded traversal from bypassing prefix
rules.

## 5. Routing

Routes are compiled once per snapshot and sorted by:

1. longest path prefix;
2. host specificity (exact host before wildcard before no host);
3. host string;
4. route name, for determinism.

Matching requires host compatibility and a **path-boundary-aware** prefix match:
`/api/users` matches `/api/users`, `/api/users/1`, but not `/api/users-archive`. If a
path/host matches but no route accepts the method, the result is 405 with an `Allow`
header. If nothing matches, 404.

Config validation rejects ambiguous route pairs: same host and path prefix with
overlapping method sets. Disjoint method sets are allowed. Hosts may be exact or
`*.example.com`; a bare `*` is rejected because it hides mistakes.

`strip_prefix` must be a prefix of `path_prefix` and end on a path boundary. The proxy
strips it before constructing the upstream URL, and never forwards the original route
prefix accidentally.

## 6. Authentication and authorization

Credentials are extracted in this order and normalized:

- `Authorization: Bearer <token>` — a JWT, unless the token starts with `gw_`, in which
  case it is treated as an API key;
- `Authorization: ApiKey <key>` or `ApiKey`/`API-Key` scheme;
- `X-API-Key: <key>`.

Multiple `Authorization` headers, conflicting keys, or an unknown scheme are rejected.
Supplying invalid credentials is rejected even when auth is optional; omitting credentials
on an optional route yields an anonymous principal.

**JWT** supports HS256 and RS256. Secrets come only from environment variables or mounted
files. The parser pins allowed algorithms, requires `exp`, enforces issuer/audience and a
leeway capped at 5 s. `sub` is preferred; `client_id` is a fallback for OAuth2 access
tokens. Scopes are read from the configured claim plus `scope`/`scopes`; roles from `roles`
and `role`. Algorithm confusion is rejected by checking the header algorithm against the
configuration and the concrete signing-method family.

**API keys** have the form `gw_<id>_<secret>`. Only the lowercase hex SHA-256 of `<secret>`
is stored. Comparison uses `crypto/subtle.ConstantTimeCompare`, including a dummy compare
for unknown IDs. Revocation and expiry are checked after the digest match.

`auth.Authorize` applies route scopes and roles. Each non-empty list must pass; `match:
any` accepts one member, `match: all` requires every member. Anonymous callers are
forbidden when auth is required.

Before forwarding, client-supplied `X-User-*` headers are stripped. For authenticated
callers the gateway sets `X-User-Id`, `X-User-Kind`, `X-User-Scopes` and `X-User-Roles`
with control characters removed and length caps. Upstreams can trust these only because
the gateway removes any client-provided values first.

Failures map to `401` for missing/invalid/expired credentials, `403` for
scope/role failures, and increment `gateway_auth_failures_total{route,reason}`. Response
messages are generic; the reason is in metrics/logs, never in the client body.

## 7. Rate limiting

A route may define a token bucket: `capacity` (burst) and `refill_per_sec` (sustained
rate). The key type is one of:

| Key | Identity |
|---|---|
| `ip` (default) | client IP, honoring trusted proxies only |
| `user` | principal ID, or owner for API keys |
| `api_key` | API-key ID |
| `header` | value of a configured header, hashed into the key |

If the chosen identity is empty, the client IP is used. Redis keys are structured as
`rl:<route>:<key_type>:<identity>` with the route bounded and header values hashed, so
arbitrary headers cannot create unbounded key cardinality.

**Redis path.** A single Lua script performs read-modify-write atomically using Redis
`TIME`; token math never happens client-side. That makes limits correct across gateway
instances without optimistic retries. The script returns limit, remaining, retry-after and
reset-after values.

**Fallback.** When Redis is unavailable, `ratelimit.LocalLimiter` is used: 64 shards, each
a mutex-protected map of buckets with a TTL-based janitor. It is an accuracy fallback, not
a distributed replacement. The global `redis.fail_open` policy and the per-route
`fail_open` override decide behavior:

- `fail_open: true` — allow the request, mark the decision `failopen`; availability over
  strictness.
- `fail_open: false` — return `503 limiter_unavailable`; Redis becomes a hard
  startup/readiness dependency. The server refuses to start when a strict route exists and
  Redis cannot be reached.

Every decision emits `X-RateLimit-Limit`, `X-RateLimit-Remaining` and (when refilling)
`X-RateLimit-Reset`. Rejections are `429` with a `Retry-After` header and a message with
the same number of seconds. Metrics distinguish `allowed`, `limited`, `error` and
`failopen`.

Tradeoff: a single Redis is a single point of failure for limit **accuracy**. Fail-open
keeps serving; the cost is temporary over-admission. Strict mode is available when the
limit is a security boundary rather than a fairness mechanism.

## 8. Upstream selection and health

Each upstream owns one `http.Transport` and one balancer pool. Strategies:

- `round_robin` — atomic rotation;
- `weighted_round_robin` — smooth WRR, so weights are spread rather than bursting;
- `least_conn` — fewest in-flight attempts, with rotation on ties.

`Acquire` skips targets that are unhealthy or excluded by the caller. Retries pass the
already-attempted target set so a retry never retries the target that just failed. The
returned target carries an in-flight counter released exactly once by the proxy.

Health has two layers:

- **Active probes** hit `health_check.path` on an interval and compare the status range.
  Healthy/unhealthy thresholds add hysteresis.
- **Passive ejection** records transport errors and qualifying 5xx responses per attempt,
  even when active checks are disabled. Successes recover the target through the same
  threshold logic.

Targets are identified by normalized URL, so state survives reloads where the URL is
unchanged. Health gauges and probe counters are exported per upstream/target.

## 9. Circuit breaker

A breaker exists per upstream and is per gateway instance. It has three states:

```text
Closed ── failure threshold or ratio ──► Open
  ▲                                      │ open timeout
  │                                      ▼
  └──── half-open successes ◄──────── Half-open
```

- **Closed:** a rolling window of buckets records success/failure. It trips when
  consecutive failures reach `failure_threshold`, or when at least `min_requests` have
  been seen and the failure ratio reaches `failure_ratio`.
- **Open:** requests fail fast with `503` and `Retry-After` equal to the remaining open
  timeout. After `open_timeout`, the next request transitions to half-open.
- **Half-open:** at most `half_open_max_probes` attempts are admitted. Enough successes
  close the breaker; any failure reopens it.

The completion callback is `sync.Once`-guarded and must be called exactly once per
admitted attempt, including retries and errors. Outcomes from requests admitted before a
trip are ignored by the closed-state recorder, so stale completions cannot reopen a
recovered breaker. Client errors (4xx) are ignored; transport errors and 5xx count. HTTP
500 counts only when `count_500: true`, because some APIs use 500 for application errors.

State is local to the process by design: a Redis round trip on every attempt would add
latency and a new failure dependency to the hot path. The tradeoff is that instances may
disagree briefly during a partial outage.

## 10. Proxy, retries and streaming

The proxy reimplements forwarding rather than using `httputil.ReverseProxy` so it can
control retries, breaker accounting, health recording, body buffering and error mapping in
one place.

Request bodies:

- `route.max_body_bytes` caps both buffered and streamed bodies; oversize is `413`.
- If retries are enabled for the method, the body is buffered up to
  `retry.body_buffer_bytes`. A larger body is streamed without retries.
- `Expect: 100-continue` is handled by the transport; bodies are replayed only when
  marked replayable.

Retry policy:

- `max_attempts` includes the first attempt;
- reasons: `connection_error`, `timeout`, or specific 5xx status codes;
- methods default to idempotent verbs (`GET`, `HEAD`, `PUT`, `DELETE`, `OPTIONS`);
- exponential backoff with jitter, capped by `max_backoff`;
- a token-bucket **retry budget** (`budget_percent`) prevents retry storms: each request
  deposits tokens and each retry spends one. At 10%, the steady-state retry volume is
  bounded relative to original traffic.

Hop-by-hop headers (`Connection`, `Keep-Alive`, `Proxy-*`, `TE`, `Trailer`,
`Transfer-Encoding`, `Upgrade`) are removed in both directions. Forwarding headers are set
from the actual peer. Upstream responses never overwrite gateway-generated
`Retry-After` on limit/breaker rejections because those are written before forwarding.

Timeouts:

- `route.timeout` bounds the retry loop and the wait for response headers.
- Once a final response starts, the timer is stopped and the body streams under client
  cancellation, so SSE and long downloads are not cut off by the route deadline.
- Transport-level dial/TLS/response-header timeouts provide a second bound.

Failures map to the stable envelope:

| Condition | Status | Code |
|---|---|---|
| no target available | 503 | `service_unavailable` |
| breaker open | 503 | `circuit_open` |
| upstream timeout | 504 | `gateway_timeout` |
| connection/DNS/TLS error | 502 | `bad_gateway` |
| request body too large | 413 | `payload_too_large` |
| limiter unavailable (strict) | 503 | `limiter_unavailable` |
| overloaded | 503 | `overloaded` |

Internal error strings, upstream URLs and credentials are logged, never returned.

## 11. Observability

Metrics are registered on a private Prometheus registry (no default globals) and exposed
only on the metrics listener. Label values are bounded by configuration: route names,
upstream names, normalized target URLs, a small status set and fixed decision enums. Raw
paths, user IDs and headers never become labels.

Key series:

```text
gateway_http_requests_total{route,method,status}
gateway_http_request_duration_seconds{route,method}
gateway_http_inflight_requests
gateway_upstream_requests_total{upstream,target,outcome}
gateway_upstream_duration_seconds{upstream,target}
gateway_upstream_retries_total{upstream,reason}
gateway_breaker_state{upstream}                 0 closed, 1 half-open, 2 open
gateway_ratelimit_decisions_total{route,decision}
gateway_auth_failures_total{route,reason}
gateway_target_healthy{upstream,target}
gateway_config_reloads_total{result}
```

Access logs are structured JSON (or text) and contain request ID, route, method,
normalized path (query stripped or redacted), status, bytes, duration, client IP,
principal log ID and error code. `Authorization`, API keys, cookies and request bodies are
never logged.

## 12. Concurrency and goroutine ownership

- Every goroutine takes a context and exits when it is cancelled: health checkers, the
  local-limiter janitor, listeners, watcher.
- Locks are short and never held across I/O. Redis calls happen outside any state lock.
- Balancer counters use atomics; smooth WRR uses a short mutex only around weight state.
- Breaker callbacks are called without the breaker lock.
- The retry budget is atomic.
- State close waits up to 5 s for health checks to stop, then closes idle connections.
- `goleak` verifies the server and admin test lifecycles.

## 13. Security posture

- Secrets only from env or mounted files; redaction is applied to the admin config dump.
- API keys are hashed with SHA-256; comparisons are constant-time.
- Admin token comparison is constant-time; the admin listener fails closed without a token.
- pprof is only on the admin listener and requires the admin token.
- Trusted proxies are explicit CIDRs/IPs; `X-Forwarded-For` is ignored from untrusted
  peers.
- Client identity headers are stripped before forwarding.
- Request paths are normalized and body sizes bounded.
- Error responses never include internal error text.

## 14. Failure matrix

| Failure | Detection | Behavior |
|---|---|---|
| Redis down, fail-open | limiter error | local fallback; `failopen` metric |
| Redis down, strict | limiter error | 503; readiness false |
| One target down | active/passive health | balancer skips it; retry may hit another |
| All targets down | health | 503 `service_unavailable` |
| Upstream 5xx burst | breaker | open; 503 `circuit_open` with `Retry-After` |
| Upstream slow | route/transport timeout | 504; retry if budget allows |
| Client disconnects | context cancellation | upstream attempt cancelled; no retry |
| Config invalid on reload | parse/validate | old snapshot keeps serving; reload failure metric |
| Gateway overloaded | concurrency semaphore | 503 `overloaded` |
| Gateway panic | recovery middleware | 500 envelope; panic metric/log |
| Shutdown signal | listener close | readiness false; drain in-flight; exit |

## 15. Tradeoffs and roadmap

- **Static targets.** No service discovery keeps routing deterministic and cheap; dynamic
  endpoints would need a separate controller and a consistency model.
- **Per-instance breakers.** No shared breaker state avoids a hot-path dependency; brief
  disagreement during outages is acceptable for most APIs.
- **Local fallback accuracy.** Fail-open favors availability; strict mode exists for
  security limits. A future option is a shared local budget coordinated through Redis.
- **Buffered retries.** Replayability requires buffering bounded bodies; large uploads are
  streamed and not retried.
- **No caching/transformation.** These belong in a separate layer; adding them here would
  complicate cache invalidation, auth and streaming semantics.

See [CHAOS.md](CHAOS.md) for the experiments that exercise this failure model.
