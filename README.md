# vyking-3p-integration-service

Go service that integrates our customer database with the **Acme Support** platform across multiple brands:

- **Contact sync** (internal): periodically pushes changed customers from `sync_queue` to Acme Support and stores the
  resulting contact id in `partner_contacts`.
- **Partner API** (public): read-only, authenticated endpoint that lets Acme Support fetch a customer's recent transactions.

Design decisions, assumptions and open questions are documented in [NOTES.md](NOTES.md).

## Requirements

- Go 1.27+
- Docker with Compose v2
- GNU make and bash (on Windows run the targets from WSL: `wsl make <target>`)

## Quick start (Docker)

```sh
make up      # build the image, start MySQL, apply Flyway migrations, start the service (waits until healthy)
curl localhost:9090/ready
make logs    # follow service logs
make down    # stop, keep data (named volume survives restarts)
make clean   # stop and delete the data volume for a fresh start
```

| Service  | Port | Notes |
|----------|------|-------|
| `app`    | 8080 (public), 9090 (internal) | distroless image, ~20 MB |
| `mysql`  | 3306 | MySQL 8.4 LTS, data in volume `vyking-3pi_mysql-data` |
| `flyway` | –    | one-shot: applies `db/migrations`, then creates the least-privilege `app` user |

To run the service on the host instead: `make deps-up` (MySQL + migrations only), then `make run`.

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

## Operational endpoints (internal port `:9090`)

| Endpoint           | Purpose                                                                 |
|--------------------|-------------------------------------------------------------------------|
| `GET /health`      | Liveness: the process is serving HTTP                                   |
| `GET /ready`       | Readiness: dependencies reachable, not shutting down                    |
| `/debug/pprof/*`   | Go profiler behind Basic auth (`PPROF_ENABLED`, `PPROF_USERNAME`, `PPROF_PASSWORD`) |

```sh
go tool pprof -top 'http://pprof:pprof-dev-only@localhost:9090/debug/pprof/heap'   # local defaults
```

## Development

```sh
make help    # list all targets
make test    # unit tests (race detector)
make build   # build bin/server
make run     # run locally with APP_ENV=local
```
