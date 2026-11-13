# vyking-3p-integration-service

Go service that integrates our customer database with the **Acme Support** platform across multiple brands:

- **Contact sync** (internal): periodically pushes changed customers from `sync_queue` to Acme Support and stores the
  resulting contact id in `partner_contacts`.
- **Partner API** (public): read-only, authenticated endpoint that lets Acme Support fetch a customer's recent transactions.

Design decisions, assumptions and open questions are documented in [NOTES.md](NOTES.md).

## Architecture

One binary, run as several replicas. Each one serves the partner API on a public port and runs the contact sync in
the background; a MySQL named lock lets only one replica sync at a time. Operational endpoints live on a separate,
cluster-internal port.

```mermaid
flowchart LR
    partner(["Acme Support<br/>support agents"])
    acmeapi(["Acme Support API<br/>/v2/contacts"])
    k8s(["probes, operators"])

    subgraph svc["vyking-3p-integration-service (every replica)"]
        direction LR
        subgraph pubport["public :8080"]
            mw["request id, access log,<br/>metrics, recover"]
            authn["JWT auth<br/>HS256, brands, scope"]
            api["GET /v1/transactions"]
        end
        subgraph intport["internal :9090 (cluster only)"]
            ops["probes: /health /ready<br/>/metrics, /debug/pprof<br/>/internal/sync/run, /status"]
        end
        txsvc["transactions service<br/>brand scope, status mapping,<br/>amounts, cursor"]
        cache[("cache<br/>15 s TTL")]
        syncer["contact sync<br/>scheduler, per-brand workers,<br/>backoff"]
        client["Acme client<br/>token bucket, retries,<br/>circuit breaker"]
        prepo["Customers, Transactions<br/>repositories"]
        srepo["Sync, Locker<br/>repositories"]
    end

    subgraph mysql["MySQL 8"]
        replica[("replica<br/>partner reads")]
        primary[("primary<br/>sync reads and writes,<br/>leader lock")]
    end

    prom["Prometheus"] --> grafana["Grafana"]

    partner -->|"Bearer partner JWT"| mw --> authn --> api --> txsvc
    txsvc <--> cache
    txsvc --> prepo --> replica
    syncer --> srepo --> primary
    syncer --> client -->|"Bearer brand API key"| acmeapi
    k8s --> ops
    ops -.->|"trigger, status"| syncer
    prom -->|"scrape /metrics"| ops
```

A sync cycle, as run by every replica on `sync.interval`, immediately again while there is a backlog, or on
`POST /internal/sync/run`:

```mermaid
sequenceDiagram
    autonumber
    participant S as Contact sync
    participant DB as MySQL primary
    participant A as Acme Support
    S->>DB: GET_LOCK(sync lock, no wait)
    alt another replica holds the lock
        DB-->>S: 0, skip this cycle
    else this replica is the leader
        S->>DB: oldest outstanding sync_queue entries per brand, minus customers backing off
        loop each customer, brands in parallel, N workers per brand
            S->>DB: load the customer and its stored contact id
            alt contact id known
                S->>A: PATCH /v2/contacts/{id}
            else new customer
                S->>A: POST /v2/contacts with external_id = customer id
                opt 409, created before a crash
                    S->>A: GET /v2/contacts?email=...
                    S->>A: PATCH /v2/contacts/{id}
                end
            end
            S->>DB: one transaction: upsert partner_contacts, mark the fetched entries processed
        end
        S->>DB: RELEASE_LOCK
    end
    Note over S,A: Any failure leaves the entries outstanding, retried next cycle or after a per-customer backoff
```

The code follows a hexagonal (ports and adapters) layout; see [Project layout](#project-layout):

- **Inbound adapter** `internal/adapter/handler`: routing, middleware, JWT check, request parsing, response mapping;
  every client error goes through `internal/apierror`.
- **Use cases** `internal/service/transactions` and `internal/service/contactsync` depend only on `internal/domain`
  and on the small interfaces (ports) declared in their own `ports.go`.
- **Outbound adapters** implement those ports: `repository` (MySQL, prepared statements), `acme` (HTTP client),
  `cache` (in-process).
- **Composition root** `internal/app` wires everything; `internal/platform` holds the infrastructure (connection pools
  and `WithTx`, logging, metrics, rate limiter, health checks).

## Requirements

- Go 1.27+
- Docker with Compose v2
- GNU make and bash (on Windows run the targets from WSL: `wsl make <target>`)

## Quick start (Docker)

```sh
make up        # build the images, start MySQL + Flyway migrations, the Acme mock and the service (waits until healthy)
make seed      # load sample customers, transactions and sync queue entries (the sync starts pushing them)
curl localhost:9090/ready
make logs      # follow service logs
make obs-up    # optional: add Prometheus (:9091) and Grafana (:3000) with the provisioned dashboard
make down      # stop, keep data (named volume survives restarts)
make clean     # stop and delete the data volume for a fresh start
```

| Service  | Port | Notes |
|----------|------|-------|
| `app`    | 8080 (public), 9090 (internal) | distroless image, ~20 MB |
| `mysql`  | 3306 | MySQL 8.4 LTS, data in volume `vyking-3pi_mysql-data` |
| `flyway` | –    | one-shot: applies `db/migrations`, then creates the least-privilege `app` user |
| `acmemock` | 8081 | stand-in for Acme Support: the documented contract, one account per API key, 60/min + burst 10 with `Retry-After`, optional latency and 5xx injection (`ACME_MOCK_*`); state at `/_debug/contacts`, counters at `/_debug/stats` |
| `prometheus` | 9091 | only with `make obs-up` (compose profile `observability`); scrapes `app:9090/metrics`, keeps 2 days |
| `grafana` | 3000 | only with `make obs-up`; anonymous viewer, the provisioned *Vyking 3P integration* dashboard is the home page (admin password `admin-dev-only`) |

To run the service on the host instead: `make deps-up` (MySQL, migrations and the Acme mock), then `make run`.

> **Docker inside WSL on Windows:** the published ports listen inside the WSL virtual machine. If `localhost:<port>`
> does not reach them from Windows, use the WSL address instead: `wsl hostname -I` (first value), for example
> `http://172.20.0.5:3000` for Grafana. The address changes when WSL restarts.

## Partner API

```http
GET /v1/transactions?email=jane.doe@example.com&status=failed,cancelled&limit=20 HTTP/1.1
Host: localhost:8080
Authorization: Bearer <token>
```

| Parameter  | Required | Description |
|------------|----------|-------------|
| `email`    | yes | customer email (case-insensitive) |
| `status`   | no  | `pending`, `completed`, `failed`, `cancelled`; comma-separated or repeated |
| `limit`    | no  | page size, default 20, max 100 |
| `brand_id` | no  | restricts the lookup to one brand; required when the email exists in several of the token's brands |
| `cursor`   | no  | `page.next_cursor` of the previous page |

```json
{
  "request_id": "01a1d02c-887f-749a-8635-0b7ec8879418",
  "customer": {"id": "1001", "brand_id": 1, "brand": "Vyking Casino", "currency": "EUR", "active": true},
  "transactions": [
    {
      "id": "42", "type": "payin", "amount": 123.45, "currency": "EUR",
      "status": "completed", "status_detail": "settled", "failure_reason": null,
      "payment_method": "PAYIN CARD",
      "created_at": "2026-11-01T10:00:00Z", "booked_at": "2026-11-01T10:01:00Z",
      "settled_at": "2026-11-01T11:00:00Z", "updated_at": "2026-11-01T11:00:00Z"
    }
  ],
  "page": {"limit": 20, "has_more": true, "next_cursor": "eyJ2IjoxLCJj..."}
}
```

- Amounts are JSON numbers in the customer's currency (exact decimals, e.g. `1000` JPY, `1.234` BHD), `null` when unknown.
- Timestamps are RFC 3339 UTC. `settled_at` is set for settled transactions only.
- `status` uses Acme Support's vocabulary; `status_detail` and `failure_reason` refine it (mapping in [NOTES.md](NOTES.md)).
- Every response carries `X-Request-ID`; errors look like
  `{"error": {"code": "customer_not_found", "message": "...", "request_id": "..."}}`.

| HTTP | Code | When |
|------|------|------|
| 400 | `invalid_parameter`, `invalid_cursor` | bad query parameter / cursor from a different query |
| 401 | `unauthorized`, `token_malformed`, `token_expired`, `token_invalid` | missing or rejected token |
| 403 | `insufficient_scope`, `brand_forbidden` | token lacks `transactions:read` / `brand_id` not in the token |
| 404 | `customer_not_found` | no such email in the token's brands |
| 409 | `customer_ambiguous` | email exists in several of the token's brands; `details.brand_ids` lists them |
| 503 | `service_unavailable` | database temporarily unavailable, retry later |

Try it with the compose stack:

```sh
make up seed
eval "$(make -s tokens)"
curl -s -H "Authorization: Bearer $TOKEN_VALID_BRAND_1" \
  'localhost:8080/v1/transactions?email=jane.doe@example.com&limit=5' | jq
```

## Contact sync

A background job in every instance; only the replica holding a MySQL named lock syncs in a given cycle. Each cycle:

1. fetches the oldest outstanding `sync_queue` entries **per brand** (so one brand's backlog cannot starve another);
2. coalesces entries per customer and pushes the customer's *current* data to Acme Support with the brand's API key
   (update if the contact id is known, otherwise create; a `409` is resolved through the email lookup);
3. stores the contact id in `partner_contacts` and marks exactly the fetched entries processed, **in one transaction**.

Entries are only marked processed after a confirmed sync: failures leave them outstanding (customers whose data Acme
rejects are retried with an exponential backoff). Cycles run every `sync.interval`, immediately again while there is a
backlog, or on demand:

```sh
make up seed                                         # stack with the Acme mock and sample data
make logs                                            # "customer synced" / "sync cycle finished" lines (run_id, outcomes)
curl -s localhost:9090/internal/sync/status | jq     # last cycle: leader, fetched, outcomes, backing off, ...
curl -s -X POST localhost:9090/internal/sync/run     # run a cycle now
curl -s localhost:8081/_debug/contacts | jq          # what the Acme mock received
```

| Variable | Purpose |
|----------|---------|
| `SYNC_ENABLED` | `false` disables the sync (e.g. API-only deployments) |
| `ACME_BASE_URL` | Acme Support API base URL (compose: the mock) |
| `ACME_API_KEY_BRAND_<n>` | API key per brand; brands without a key are not synced and their entries stay outstanding |

Rate limit, retry, backoff and concurrency settings are in the `acme` and `sync` sections of `config/*.yaml`.

## Observability

- **Logs:** JSON (`LOG_FORMAT=json`) with `request_id` on every request line and `run_id` / `brand_id` / `customer_id`
  on sync lines. Emails are masked and query strings are never logged.
- **Metrics:** Prometheus format at `GET :9090/metrics` (internal port only).
- **Dashboard:** `make obs-up` adds Prometheus (`localhost:9091`) and Grafana (`localhost:3000`, anonymous viewer) with
  a provisioned *Vyking 3P integration* dashboard.

| Metric | Labels | Use |
|--------|--------|-----|
| `vyking_http_requests_total`, `vyking_http_request_duration_seconds` | `server`, `route`, `code` | traffic, errors, latency per route pattern |
| `vyking_partner_lookups_total` | `outcome` | `ok` or the error code (`customer_not_found`, `customer_ambiguous`, ...) |
| `vyking_auth_failures_total` | `code` | rejected tokens (`token_expired`, `token_invalid`, ...) |
| `vyking_cache_requests_total` | `result` | cache hit ratio |
| `vyking_acme_requests_total`, `vyking_acme_request_duration_seconds` | `brand`, `op`, `status` | Acme Support answers and latency |
| `vyking_acme_events_total` | `brand`, `event` | `rate_limited`, `circuit_opened`, `key_refused` |
| `vyking_sync_customers_total` | `brand`, `outcome` | `created`, `updated`, `recovered`, `rejected`, `partner_unavailable`, ... |
| `vyking_sync_oldest_outstanding_age_seconds` | `brand` | how far behind the sync is (alert when it keeps growing) |
| `vyking_sync_cycles_total`, `vyking_sync_cycle_duration_seconds`, `vyking_sync_customers_backing_off`, `vyking_sync_orphan_entries`, `vyking_sync_unconfigured_entries` | | sync health |
| `go_sql_*` | `db_name` | connection pool usage per pool (primary, replica) |

## Partner authentication

The partner API expects `Authorization: Bearer <token>`, where the token is an HS256 JWT issued by us that lists the brands
the partner may access (details and alternatives in [NOTES.md](NOTES.md#partner-authentication-deliberately-unspecified-by-the-task)).
Tokens for local testing are signed with the key from the active config:

```sh
make -s tokens                                    # TOKEN_VALID_BRAND_1=..., TOKEN_EXPIRED=..., TOKEN_ALG_NONE=..., ...
eval "$(make -s tokens)"                          # export them into the shell
go run ./scripts/tokengen -brands 1,2 -ttl 24h    # a single custom token
```

| Variable | Purpose |
|----------|---------|
| `AUTH_KEY_ID`, `AUTH_JWT_SECRET` | current verification key (`kid` + secret, ≥ 32 bytes) |
| `AUTH_PREVIOUS_KEY_ID`, `AUTH_PREVIOUS_JWT_SECRET` | previous key, kept during rotation (optional) |

## Sample data

`make seed` loads deterministic sample data into the compose database (it connects as MySQL root because it writes
tables the service may not, and refuses non-local hosts). Options: `go run ./scripts/seed -h`.

Besides ~200 random customers it creates well-known fixtures covering the edge cases of the task:

| Customer | Brand | Email | Purpose |
|----------|-------|-------|---------|
| 1001 | 1 | `jane.doe@example.com` | EUR, every transaction status |
| 1002 | 2 | `jane.doe@example.com` | same email in another brand (ambiguous for multi-brand partners) |
| 1003 | 1 | `taro@example.jp` | JPY (0 decimals); has a stale `partner_contacts` id |
| 1004 | 1 | `ali@example.bh` | BHD (3 decimals) |
| 1005 | 2 | `sam@example.com` | USD, whose minor units are NULL in the reference data |
| 1006 | 1 | – | no email: cannot be looked up |
| 1007 | 1 | `inactive@example.com` | inactive customer |
| 1008 | 1 | `minimal@example.com` | only mandatory columns |
| 1009 | 1 | `heavy@example.com` | 250 transactions, for pagination |
| 1010 | 3 | `poker@example.com` | brand without Acme Support credentials |

Every fixture has outstanding `sync_queue` entries, some customers several (coalescing) and some an already processed one.

## Database

- `db/migrations` contains the task's schema and reference data as Flyway migrations. They exist for **local development and
  tests only**; the production schema is owned by another team and must never be migrated by this service.
- `db/callbacks/afterMigrate__app_user.sql` creates the `app` user the service connects with. It can read everything but
  write only `sync_queue` and `partner_contacts`, so the "read-only except two tables" rule is enforced by the database.
- The service waits up to `database.startup_timeout` for the database before opening its listeners, then exits non-zero
  so the orchestrator restarts it. `/ready` reports each connection pool separately.
- Every query is a named constant at the top of its repository file (`internal/adapter/repository`) and is prepared
  once at start-up, so a broken query or a missing grant stops the start instead of failing requests. Id lists are
  bound as one JSON array parameter; no SQL text is built from request data.

## Configuration

Configuration lives in per-environment YAML files under [`config/`](config):

| Variable      | Purpose                                              | Default          |
|---------------|------------------------------------------------------|------------------|
| `APP_ENV`     | Selects `config/<env>.yaml` (`local`, `dev`, `staging`, `prod`) | `local` |
| `CONFIG_DIR`  | Directory containing the environment files           | `config`         |
| `CONFIG_PATH` | Explicit config file path (overrides the two above)  | –                |

Values in the YAML may reference environment variables: `${VAR}` (required) or `${VAR:-default}`. Secrets are only ever
provided through environment variables. Unknown keys and invalid values fail fast at startup.

Main environment variables (see the YAML files for every tunable):

| Variable | Purpose |
|----------|---------|
| `DB_HOST`, `DB_PORT`, `DB_NAME`, `DB_USER`, `DB_PASSWORD` | primary database (writes and sync reads) |
| `DB_REPLICA_ENABLED`, `DB_REPLICA_HOST`, `DB_REPLICA_PORT`, `DB_REPLICA_USER`, `DB_REPLICA_PASSWORD` | optional read replica for partner API reads |
| `LOG_LEVEL`, `LOG_FORMAT` | `debug`/`info`/`warn`/`error`, `text`/`json` |
| `PPROF_ENABLED`, `PPROF_USERNAME`, `PPROF_PASSWORD` | profiler on the internal port |
| `PUBLIC_ENABLED` | `false` runs the instance as a sync-only worker |
| `CACHE_ENABLED` | in-process cache of partner lookups (15s TTL) |

## Operational endpoints (internal port `:9090`)

| Endpoint           | Purpose                                                                 |
|--------------------|-------------------------------------------------------------------------|
| `GET /health`      | Liveness: the process is serving HTTP                                   |
| `GET /ready`       | Readiness: dependencies reachable, not shutting down                    |
| `GET /metrics`     | Prometheus metrics (see "Observability")                                |
| `POST /internal/sync/run` | Start a contact sync cycle now                                   |
| `GET /internal/sync/status` | Summary of the last sync cycle                                 |
| `/debug/pprof/*`   | Go profiler behind Basic auth (`PPROF_ENABLED`, `PPROF_USERNAME`, `PPROF_PASSWORD`) |

The internal port must only be reachable inside the cluster (ClusterIP service, never the ingress).

```sh
go tool pprof -top 'http://pprof:pprof-dev-only@localhost:9090/debug/pprof/heap'   # local defaults
```

## Development

```sh
make help              # list all targets
make test              # unit tests (race detector)
make test-integration  # integration tests against a throwaway MySQL (see below)
make build             # build bin/server
make run               # run on the host with APP_ENV=local (after `make deps-up`)
```

### Tests

- **Unit tests** (`make test`, race detector on): table-driven with testify and hand-written fakes. Time-dependent code
  (rate limiter, cache TTL, sync scheduler, shutdown) is tested deterministically with `testing/synctest`; HTTP clients
  and handlers run against `httptest`.
- **Integration tests** (`make test-integration`) start a **separate, throwaway** stack: MySQL on tmpfs (compose project
  `vyking-3pi-it`, port 33306) migrated by the real Flyway, plus an Acme mock (port 18081). They run
  `go test -tags=integration -p 1` and **always** tear the containers down. A failure exits non-zero, so CI can run the
  very same target, and the development database is never touched. They cover:
  - the complete service over HTTP (config, auth, partner API, contact sync against the mock), connected as the
    least-privilege `app` user;
  - database-level checks: atomic recording, leader election when the lock session dies, fair per-brand fetching, and
    the query plan (rows read per page).
- **End-to-end checks** were run against the full compose stack after every major part: auth matrix, pagination
  against SQL, outage and recovery, graceful shutdown, SIGKILL mid-sync, two replicas, Acme throttling and failures,
  refused keys, metrics. NOTES.md records the findings and the fixes they led to.

## Project layout

```
cmd/server/                 entrypoint (+ `healthcheck` subcommand for distroless images)
internal/app/               composition root: wiring, start-up, graceful shutdown
internal/config/            per-environment YAML with ${VAR:-default} expansion and validation
internal/domain/            business types and rules (status mapping, amounts, contacts) - no dependencies
internal/service/           use cases with their ports: transactions (partner API), contactsync (sync)
internal/adapter/           handler (HTTP in), repository (MySQL), acme (Acme Support client), cache
internal/auth/              partner JWT verification
internal/apierror/          the client-facing error model
internal/platform/          logging, database (pools, WithTx), ratelimit, health, metrics, requestid
db/                         Flyway migrations + least-privilege user callback (local/test only)
config/                     local, dev, staging, prod configuration
scripts/                    seed, tokengen, acmemock (development tools)
test/integration/           end-to-end tests against the throwaway stack
deploy/observability/       Prometheus scrape config, Grafana provisioning and dashboard
```
