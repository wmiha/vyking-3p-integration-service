# NOTES

Questions, assumptions, identified problems, decisions and trade-offs. This document grows with the implementation.

## Decisions and trade-offs

### Configuration

- **Per-environment YAML + environment variables.** `config/<APP_ENV>.yaml` holds every tunable (timeouts, pool sizes,
  rate limits, schedules) so it can be reviewed and diffed; values may reference `${VAR}` / `${VAR:-default}` so secrets and
  per-deployment values come from the environment (Kubernetes ConfigMap + Secret). Real credentials never live in the repo.
- **Placeholders are expanded per YAML scalar, not on raw text.** A secret containing YAML syntax (`:`, `#`, quotes, newlines)
  cannot change the document structure. Unquoted placeholders take the type of their value (`${PORT:-3306}` → int), quoted
  ones always stay strings.
- **Strict decoding.** Unknown keys are rejected and all validation errors are reported together, so a typo in a tuning
  parameter fails the deployment instead of silently running with a zero value.

### Database access

- **Flyway is for local development and tests only.** The schema is "fixed and owned elsewhere"; running migrations against
  production would also write `flyway_schema_history`, a table we are not allowed to write. The migrations reproduce the given
  schema verbatim (only wrapped comments were joined) so local and test runs behave like production.
- **Least privilege enforced by MySQL.** The `app` user (created by a Flyway callback locally) has `SELECT` on the schema,
  `INSERT, UPDATE` on `partner_contacts` and `UPDATE` on `sync_queue`, nothing else, not even `DELETE`. The task's write
  restriction is therefore guaranteed by the database, not just by code review. Production should use the same grants.
- **Primary/replica split.** Two pools with independent sizing: the primary takes writes and *all* sync reads; the replica
  (optional, falls back to the primary) takes partner API reads. The sync must never read from a replica: a lagging customer
  snapshot could be pushed to Acme and the queue entry marked processed, silently losing the newer change.
- **Pool tuning lives in config** (`max_open_conns`, `max_idle_conns`, `conn_max_lifetime`, `conn_max_idle_time`), per pool
  and per environment. Total connections = pods × `max_open_conns`; this must stay below the server's `max_connections`.
  `conn_max_lifetime` (30m) is kept below typical proxy/load-balancer idle timeouts.
- **Timezones.** `DATETIME` columns carry no zone. We assume UTC (configurable per pool) and always write `UTC_TIMESTAMP()`.
- **One transaction helper.** Every multi-statement write goes through `database.WithTx`: commit on success, rollback on
  error or panic, and bounded, jittered retries of the *whole* unit of work on deadlock (1213) or lock wait timeout (1205).
  There is no partially applied state.
- **Start-up.** The service waits up to `database.startup_timeout` for its pools before opening listeners, then exits
  non-zero, which makes a misconfiguration visible as a crash loop instead of a pod that is silently never ready.

### Error model

- One package (`internal/apierror`) owns the client-facing error shape:
  `{"error": {"code", "message", "request_id", "details"}}`. Codes are stable, snake_case and only ever added, never renamed,
  so Acme Support can map them reliably.
- Services return domain errors; only the HTTP layer translates them to API errors. Anything unexpected becomes
  `500 internal_error` with a generic message, and the internal cause is logged, never serialized.
- Dependency outages (database, cache) are reported as `503 service_unavailable` rather than 500 so clients know a retry
  is appropriate.
- Every response carries `X-Request-ID` (UUIDv7, time-ordered). A caller-supplied id is reused only if it is a short
  `[A-Za-z0-9._-]` token, which prevents header/log injection.

### Runtime, probes and shutdown

- **Two listeners.** The public server (`:8080`) only serves the partner API. Probes, metrics, profiling and the sync
  trigger live on an internal server (`:9090`) that is exposed through a ClusterIP service only and never through the
  ingress. This is also how the sync satisfies "must not be publicly accessible".
- **Liveness vs readiness.** `/health` only proves the process serves HTTP, so a database outage does not cause restart
  loops. `/ready` checks dependencies and turns 503 while draining.
- **Graceful shutdown.** The first SIGTERM flips readiness to 503 and waits `drain_delay` so the load balancer stops sending
  traffic, then drains the public server, then the internal server, all within `shutdown_timeout`. A second signal forces an
  immediate exit (logged). `terminationGracePeriodSeconds` must exceed `drain_delay + shutdown_timeout`.
- Access logs record the matched route, never the query string, because it carries customer emails.
- **Profiling.** `net/http/pprof` is mounted on the internal server only, behind HTTP Basic auth (credentials from
  env/secret, compared in constant time, min. 12 characters). It is enabled by default only in `local`. Authentication runs
  *before* routing, so unauthenticated callers cannot discover routes through 404/405/redirect answers. Long CPU profiles and
  traces extend their own write deadline, and are cut short when shutdown starts so they can never hold a pod past its grace
  period. Basic auth over plain HTTP is acceptable only because the port is cluster-internal; in production we would rather
  `kubectl port-forward` or put mTLS in front of it. Errors produced by the pprof handlers themselves (unknown profile,
  concurrent CPU profile) remain plain text.
- Known limitation: requests rejected by `net/http` itself before routing (malformed request line, path-cleaning
  redirects) get the standard library's plain-text responses rather than the JSON error envelope.
