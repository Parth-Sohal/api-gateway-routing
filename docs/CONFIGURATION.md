# Configuration reference

The gateway reads one strict YAML document. Unknown fields are rejected, so typos fail at
startup and on reload instead of being silently ignored. Durations accept Go duration
strings (`250ms`, `15s`, `2m`) or an integer number of seconds (`30`). An empty duration
(`""`) is zero; zero usually means "use the default".

Validate without starting listeners:

```bash
go run ./cmd/gateway -config configs/gateway.yaml -validate
```

Defaults are applied by `Parse`/`LoadFile` and again by `Validate` for Go-constructed
configs. Validation aggregates every problem into one error.

## Top level

```yaml
server:   {...}
admin:    {...}
metrics:  {...}
redis:    {...}
auth:     {...}
log:      {...}
upstreams: [...]
routes:    [...]
```

At least one route and one upstream are required. Every route must reference an existing
upstream. Route names and upstream names must be unique.

## `server`

| Field | Type | Default | Meaning |
|---|---|---|---|
| `addr` | string | `:8080` | public listener address |
| `read_header_timeout` | duration | `10s` | header read deadline |
| `read_timeout` | duration | `30s` | whole request read deadline |
| `write_timeout` | duration | `0` | response write deadline; 0 preserves streaming |
| `idle_timeout` | duration | `90s` | keep-alive idle timeout |
| `max_header_bytes` | int | `16384` | maximum request header size (min 1024) |
| `shutdown_grace` | duration | `15s` | drain deadline on SIGTERM |
| `trusted_proxies` | []string | empty | CIDRs/IPs allowed to supply forwarded client IPs |
| `max_concurrent_requests` | int | `0` | in-flight limit; 0 disables, >0 rejects excess with 503 |

Timeouts must not be negative. `trusted_proxies` entries must be valid IPs or CIDRs.
`max_concurrent_requests` must not be negative.

## `admin`

| Field | Type | Default | Meaning |
|---|---|---|---|
| `enabled` | bool | `true` | start the admin listener |
| `addr` | string | `127.0.0.1:9090` | admin listener address |
| `token_env` | string | `GATEWAY_ADMIN_TOKEN` | env var holding the admin bearer token |
| `read_header_timeout` | duration | `5s` | header read deadline |
| `shutdown_grace` | duration | `10s` | admin drain deadline |

When enabled, `addr` and `token_env` are required. The token must be non-empty at runtime;
the admin handler refuses to start without it. Every endpoint except `/healthz`, `/readyz`
and `/dashboard` requires `Authorization: Bearer <token>`.

## `metrics`

| Field | Type | Default | Meaning |
|---|---|---|---|
| `enabled` | bool | `true` | start the metrics listener |
| `addr` | string | `127.0.0.1:9100` | metrics listener address |
| `path` | string | `/metrics` | scrape path, must start with `/` |

The metrics listener exposes only the private registry (gateway, Go and process
collectors). Use network policy to keep it off the public internet.

## `redis`

Redis is contacted only when at least one route defines a `rate_limit`. Without limits the
block is optional. With a strict limit (`fail_open: false`) Redis becomes a hard
startup/readiness dependency.

| Field | Type | Default | Meaning |
|---|---|---|---|
| `addr` | string | `127.0.0.1:6379` | Redis address |
| `password_env` | string | empty | env var holding the password |
| `db` | int | `0` | database index, must be >= 0 |
| `pool_size` | int | `64` | connection pool size, must be >= 1 |
| `min_idle_conns` | int | `8` | idle connections; 0..pool_size |
| `dial_timeout` | duration | `2s` | connection timeout |
| `read_timeout` | duration | `500ms` | socket read timeout |
| `write_timeout` | duration | `500ms` | socket write timeout |
| `op_timeout` | duration | `20ms` | per-command deadline, at least 1ms |
| `fail_open` | bool | `true` | global fallback policy for limiter errors |

`password_env` is read at startup; the password is never stored in the config or logged.
When Redis is unavailable and `fail_open` is true, the local sharded limiter answers and
the decision is recorded as `failopen`. When false, the gateway returns 503 and readiness
fails.

## `auth`

### `auth.jwt`

| Field | Type | Default | Meaning |
|---|---|---|---|
| `algorithms` | []string | inferred | accepted algorithms: `HS256`, `RS256` |
| `hs256_secret_env` | string | empty | env var holding the HS256 secret |
| `hs256_secret_file` | string | empty | file holding the HS256 secret (trailing CR/LF trimmed) |
| `rs256_public_key_file` | string | empty | PEM public key, PKCS#1 key or certificate |
| `issuer` | string | empty | required `iss`, when set |
| `audience` | []string | empty | accepted `aud` values; one must match |
| `leeway` | duration | `1s` | clock skew allowance, 0..5s |
| `scopes_claim` | string | `scope` | primary claim name for scopes |

If `algorithms` is omitted, it is inferred from the configured secret/key fields. HS256
requires exactly one of `hs256_secret_env` or `hs256_secret_file`. RS256 requires
`rs256_public_key_file`. Tokens must carry `exp`; `sub` is preferred, with `client_id` as
an OAuth2 fallback. Scopes are also read from `scopes`; roles from `roles` and `role`.
Algorithm confusion is rejected.

### `auth.api_keys`

| Field | Type | Default | Meaning |
|---|---|---|---|
| `id` | string | required | key ID used in `gw_<id>_<secret>`; no `_` or spaces |
| `owner` | string | ID | owner identity used for `key: user` limits |
| `hash` | string | required | lowercase hex SHA-256 of `<secret>` (64 chars) |
| `scopes` | []string | empty | granted scopes |
| `roles` | []string | empty | granted roles |
| `revoked` | bool | `false` | reject the key |
| `expires_at` | RFC3339 time | empty | reject after this instant |

Mint a hash:

```bash
go run ./cmd/token -hash-secret 'the-secret-part'
```

The plaintext secret is never stored in YAML. The key presented by the client is
`gw_<id>_<secret>`, where `<secret>` is what was hashed.

## `log`

| Field | Type | Default | Meaning |
|---|---|---|---|
| `level` | string | `info` | `debug`, `info`, `warn`, `error` |
| `format` | string | `json` | `json` or `text` |
| `access_log` | bool | `true` | emit one structured line per request |

Access logs include the request ID, route, method, normalized path, status, bytes,
duration, client IP and principal log ID. They never include `Authorization`, API keys,
cookies or request bodies.

## `upstreams[]`

| Field | Type | Default | Meaning |
|---|---|---|---|
| `name` | string | required | unique upstream name |
| `targets` | []Target | required, min 1 | backend instances |
| `balancer` | string | `round_robin` | `round_robin`, `weighted_round_robin`, `least_conn` |
| `transport` | object | see below | per-upstream connection pool |
| `health_check` | object | see below | active probes and passive thresholds |
| `breaker` | object | see below | circuit breaker |
| `retry` | object | see below | retry policy |

### `targets[]`

| Field | Type | Default | Meaning |
|---|---|---|---|
| `url` | string | required | `http://` or `https://` base URL |
| `weight` | int | `1` | relative weight for WRR; must not be negative |

Target URLs must not contain userinfo, a query or a fragment, and duplicates are rejected.
The normalized URL is the target ID used by health, metrics and breaker state.

### `transport`

| Field | Type | Default | Meaning |
|---|---|---|---|
| `max_idle_conns` | int | `1024` | total idle connections |
| `max_idle_conns_per_host` | int | `256` | idle connections per target |
| `max_conns_per_host` | int | `0` | hard cap; 0 means unlimited |
| `idle_conn_timeout` | duration | `90s` | idle connection lifetime |
| `dial_timeout` | duration | `1s` | TCP dial timeout, must be positive |
| `response_header_timeout` | duration | `10s` | wait for response headers |
| `tls_handshake_timeout` | duration | `10s` | TLS handshake deadline |
| `expect_continue_timeout` | duration | `1s` | wait before sending a buffered body |

Negative connection counts and negative timeouts are rejected.

### `health_check`

| Field | Type | Default | Meaning |
|---|---|---|---|
| `enabled` | bool | `false` | run active probes |
| `path` | string | `/healthz` | probe path, must start with `/` |
| `interval` | duration | `5s` | probe period |
| `timeout` | duration | `1s` | per-probe deadline |
| `healthy_threshold` | int | `2` | consecutive successes to restore |
| `unhealthy_threshold` | int | `3` | consecutive failures to eject |
| `expected_status_min` | int | `200` | accepted status range start |
| `expected_status_max` | int | `399` | accepted status range end |

Interval and timeout must be positive; thresholds at least 1; the status range must be
within 100..599 and ordered. Passive ejection records failures from real traffic even when
`enabled: false`.

### `breaker`

| Field | Type | Default | Meaning |
|---|---|---|---|
| `enabled` | bool | `true` | enable the breaker |
| `failure_threshold` | int | `5` | consecutive failures that trip |
| `failure_ratio` | float | `0.5` | failure ratio in the rolling window, (0,1] |
| `min_requests` | int | `20` | minimum samples before ratio trips |
| `window` | duration | `10s` | rolling window, must be positive |
| `buckets` | int | `10` | window buckets, 1..100 |
| `open_timeout` | duration | `5s` | time before half-open |
| `half_open_max_probes` | int | `3` | concurrent probe cap |
| `half_open_successes` | int | `2` | successes needed to close |
| `count_500` | bool | `false` | count HTTP 500 as a failure |

Transport errors and 5xx responses count; 4xx are ignored. While open, requests fail fast
with 503 and `Retry-After`. The breaker is per upstream and per process.

### `retry`

| Field | Type | Default | Meaning |
|---|---|---|---|
| `enabled` | bool | `true` | enable retries |
| `max_attempts` | int | `2` | total attempts including the first, 1..10 |
| `on` | []string | `[connection_error, 502, 503, 504]` | retry reasons |
| `methods` | []string | idempotent verbs | methods eligible for retry |
| `base_backoff` | duration | `50ms` | initial backoff |
| `max_backoff` | duration | `1s` | backoff cap |
| `body_buffer_bytes` | int | `65536` | max buffered body for replay |
| `budget_percent` | int | `10` | retry budget, 1..100 |

Valid `on` values are `connection_error`, `timeout`, and any status 500..599. Backoff
values must be non-negative, with base <= max. Only the configured methods are retried;
bodies larger than `body_buffer_bytes` stream without retry. Each request deposits budget
tokens and each retry spends one, bounding retry amplification.

## `routes[]`

| Field | Type | Default | Meaning |
|---|---|---|---|
| `name` | string | required | unique route name; metric/log label |
| `match` | object | required | host, path prefix, methods |
| `upstream` | string | required | existing upstream name |
| `strip_prefix` | string | empty | prefix removed before forwarding |
| `preserve_host` | bool | `false` | forward the original Host header |
| `auth` | object | optional | authentication/authorization policy |
| `rate_limit` | object | none | token-bucket rule |
| `timeout` | duration | `30s` | retry loop and response-header deadline |
| `max_body_bytes` | int64 | `1048576` | request body cap |

### `match`

| Field | Type | Default | Meaning |
|---|---|---|---|
| `host` | string | empty | exact host or `*.example.com` |
| `path_prefix` | string | `/` | boundary-aware path prefix |
| `methods` | []string | any | allowed methods; `*` also means any |

`path_prefix` must start with `/` and be normalized (no `//`, `.` or `..`). Hosts are
lowercased; a bare `*` is rejected. Route pairs with the same host/prefix and overlapping
methods are rejected as ambiguous. Longest prefix wins; exact host beats wildcard beats no
host.

`strip_prefix` must start with `/`, be a prefix of `path_prefix`, and end on a path
boundary. `/api` is valid for `/api/users`; `/ap` is not.

### `auth`

| Field | Type | Default | Meaning |
|---|---|---|---|
| `required` | bool | `false` | reject anonymous callers |
| `methods` | []string | both when required | `jwt`, `api_key` |
| `scopes` | []string | empty | required scopes |
| `roles` | []string | empty | required roles |
| `match` | string | `any` | `any` or `all` within each list |

When `required` is true and `methods` is omitted, both JWT and API key are accepted,
subject to availability. Validation rejects a required route whose configured methods
have no usable verifier. Scopes/roles require `required: true`. Supplied invalid
credentials are rejected even on optional routes; missing credentials yield an anonymous
principal.

### `rate_limit`

| Field | Type | Default | Meaning |
|---|---|---|---|
| `capacity` | int64 | required | burst size, at least 1 |
| `refill_per_sec` | float | required | sustained rate, positive |
| `key` | string | `ip` | `ip`, `user`, `api_key`, `header` |
| `header` | string | empty | header name required when key is `header` |
| `fail_open` | bool | Redis default | per-route override |

`user` uses the principal ID (owner for API keys), falling back to IP when anonymous.
`api_key` uses the key ID. `header` hashes the header value into the Redis key, so
cardinality stays bounded.

## Secrets and environment

| Variable | Used by |
|---|---|
| `GATEWAY_ADMIN_TOKEN` | default `admin.token_env` |
| `GATEWAY_JWT_SECRET` | example `hs256_secret_env` |
| any name in `redis.password_env` | Redis password |
| any name in `auth.jwt.hs256_secret_env` | JWT signing secret |

Secrets may alternatively be mounted files (`hs256_secret_file`). They are read at
startup and never written to logs or the admin config response.

## Reload rules

`POST /admin/reload` or a file change rebuilds routes, auth, limits and upstream runtimes.
The new snapshot must parse and validate; otherwise the old snapshot keeps serving. Fixed
at startup: listener addresses, admin/metrics listeners, Redis client and limiter mode.
Changing those requires a restart.

## Complete example

See [`configs/gateway.yaml`](../configs/gateway.yaml) for the local configuration and
[`configs/gateway.compose.yaml`](../configs/gateway.compose.yaml) for the compose network.
The chaos overlay is
[`configs/gateway.toxiproxy.yaml`](../configs/gateway.toxiproxy.yaml).
