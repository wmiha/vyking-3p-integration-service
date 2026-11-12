# vyking-3p-integration-service

Go service that integrates our customer database with the **Acme Support** platform across multiple brands:

- **Contact sync** (internal): periodically pushes changed customers from `sync_queue` to Acme Support and stores the
  resulting contact id in `partner_contacts`.
- **Partner API** (public): read-only, authenticated endpoint that lets Acme Support fetch a customer's recent transactions.

Design decisions, assumptions and open questions are documented in [NOTES.md](NOTES.md).

## Requirements

- Go 1.27+
- GNU make and bash (on Windows run the targets from WSL: `wsl make <target>`)

## Configuration

Configuration lives in per-environment YAML files under [`config/`](config):

| Variable      | Purpose                                              | Default          |
|---------------|------------------------------------------------------|------------------|
| `APP_ENV`     | Selects `config/<env>.yaml` (`local`, `dev`, `staging`, `prod`) | `local` |
| `CONFIG_DIR`  | Directory containing the environment files           | `config`         |
| `CONFIG_PATH` | Explicit config file path (overrides the two above)  | –                |

Values in the YAML may reference environment variables: `${VAR}` (required) or `${VAR:-default}`. Secrets are only ever
provided through environment variables. Unknown keys and invalid values fail fast at startup.

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
