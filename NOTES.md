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
