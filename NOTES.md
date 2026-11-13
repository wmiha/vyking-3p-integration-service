# NOTES

The summary lists the questions, assumptions, problems and decisions in short form. The sections after it explain each
decision, its alternatives and its trade-offs. "Load and scalability" and "What remains" are at the end.

## Summary

### Questions we would ask

*Business and data*
1. Is the status mapping right? In particular, reversed and chargeback are reported as `completed`, approved and booked
   as `pending`. May risk declines be disclosed to a third party, or must they stay a generic `declined`?
2. Is there a real settlement timestamp anywhere? `TRX_dBooked` means "accepted for processing".
3. Transactions have no currency. Can a customer's currency change? Are payouts and adjustments stored as negative
   amounts? Why can amounts be NULL?
4. What does "recent" mean for Acme: a time window, a maximum history?
5. Should inactive customers be synced and returned? (We do both.)
6. Who writes `sync_queue`, and in the same transaction as the customer change (a true outbox)? What are the expected
   volumes: brands, customers per brand, queue inflow, partner API QPS and latency SLO?
7. Are the zone-less `DATETIME` columns UTC?
8. Can the schema owners add the recommended indexes, or give the service a status column or its own schema?

*Authentication*

9. How can Acme Support send credentials: a static bearer header or OAuth 2.0 client credentials? One token per brand,
   or one covering several? What lifetime, rotation and revocation process?
10. Should inbound requests be rate-limited per partner?

*Acme Support API*

11. Can a `409` return the existing contact id, or can contacts be looked up by `external_id`?
12. What does the email lookup return, and what does it answer when nothing matches?
13. Is PATCH a full replace or a partial update, and how is a field cleared?
14. Are emails unique per account (contacts created manually by agents)?
15. Is there a DELETE endpoint, for GDPR erasure and for customers who change brand?
16. Which phone and country formats are accepted?
17. Is an `Idempotency-Key` header supported?
18. Is a bulk import available for the initial backfill? About 50 contacts per minute per brand means about 33 hours per
    100k customers.

### Assumptions we made to proceed

- The schema is fixed and owned elsewhere. We run its migrations only locally and in tests, and we write only
  `sync_queue.SYQ_dProcessed` and `partner_contacts`. The second point is enforced by the database user's grants.
- `DATETIME` values are UTC.
- A transaction's currency is the customer's current currency. Amounts are returned as stored; NULL stays `null`.
- Unknown currency minor units fall back to ISO 4217, and an amount is never guessed.
- Partners receive HS256 JWTs that we issue, listing their brands and the `transactions:read` scope.
- An email lookup is case-insensitive but otherwise exact; emails are unique only per brand.
- Brand API keys come from configuration and secrets; brands without a key are not synced.
- Service logs are shipped to central storage and retained; they are the sync's audit trail, since we may not write
  any other table.
- Acme's lookup answers `{"id": ...}` or `404`, and PATCH accepts the full contact.
- 55 requests per minute per brand leaves headroom under Acme's 60/min in case other clients share the key.

### Potential problems and ambiguities identified

- **The schema cannot express parts of the required response:** there is no settlement timestamp, no failure reason and
  no transaction currency, and USD has NULL minor units. See "Partner API: data gaps".
- **Customer lookup by email:** emails are unique per brand only, so a multi-brand token can be ambiguous. Customers
  without email cannot be looked up. The default collation also ignores accents and would return another customer
  (fixed).
- **`sync_queue` has no status, attempts or error columns**, so failed entries cannot be parked durably. There is also
  no index on `SYQ_dProcessed`: every poll scans a table that only grows.
- **A `409` from Acme does not carry the contact id, and lookup works by email only.** A crash between Acme's create
  and our write can leave a customer unresolvable. We reproduced this for a customer without email.
- **Rate limits:** Acme's per-key limit caps throughput and is shared by all replicas, hence the leader election.
- **No DELETE in Acme's API:** erasure and brand moves cannot be propagated.
- **Emails in query strings** are PII in proxy logs. Inactive customers are visible to support (to confirm).
- **A query-plan trap.** Joining the tiny lookup tables made MySQL sort a customer's whole history on every page. This
  was found and fixed, and a test now guards it.

### Key decisions and trade-offs

- **Hexagonal layout, stdlib first.** Small consumer-declared interfaces and one place for client errors (`apierror`).
  Dependencies are limited to the MySQL driver, JWT, YAML, testify and the Prometheus client.
- **Two listeners.** The public port serves only the partner API. Probes, metrics, pprof (Basic auth) and the sync
  trigger live on an internal port.
- **The sync never drops an entry**, and its delivery is at-least-once with idempotent effects. It uses leader election
  through a MySQL named lock, fair per-brand batches, per-brand rate limits and circuit breakers. Recording happens in
  one transaction.
- **Ambiguous emails return `409 customer_ambiguous`** with the brand ids. Other brands' customers answer `404`.
- **Keyset pagination** uses a cursor bound to the customer and filter, plus a short-TTL cache keyed by the caller's
  brand scope.
- **Primary/replica pools.** The sync stays on the primary; partner reads use the replica.
- **Throwaway integration databases** are migrated by the real Flyway, and each part was verified against the running
  compose stack.

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
- **The replica is a hard dependency when enabled.** Start-up waits for it and `/ready` fails without it, so a replica
  outage takes partner reads down even though the primary could serve them. We would rather point `DB_REPLICA_HOST` at
  an endpoint that fails over by itself (RDS reader endpoint, ProxySQL) than build read failover into the service.
- **Prepared statements, prepared once.** Every repository keeps its SQL as named constants at the top of its file
  (`sqlSelectCustomersByEmail`, `sqlUpsertPartnerContact`, ...) and prepares them when it is created. `database/sql`
  re-prepares a statement on each pooled connection that runs it, so the server parses each query once per connection,
  and each later call is one binary-protocol round trip with the values sent apart from the SQL text. A broken query or
  a missing grant fails the start-up, not the first request; that is why the service now prepares after waiting for
  the database and before opening its listeners. An integration test asserts that repository calls cause no
  `COM_STMT_PREPARE`.
  - *Lists without dynamic SQL.* Brand, status and backoff lists are bound as one JSON array and expanded with
    `JSON_TABLE` (`IN (SELECT ...)`, `NOT EXISTS (...)`), which MySQL materializes once per execution. Measured on 100k
    outstanding entries with 500 excluded customers: `NOT EXISTS` matches a literal `NOT IN` list (~0.43 s), while
    `MEMBER OF`, shorter to write, re-parses the JSON for every row (~6 s) and was rejected. On a 200k-transaction
    history with a rare status filter (the worst case), the status filter costs about the same as `MEMBER OF` and the
    page still walks the index backwards and stops early.
  - *Marking entries processed* is one primary-key `UPDATE` per entry inside the recording transaction. It locks exactly
    those rows; a set-based update over a list would leave the access path to the optimizer, and an `UPDATE` that scans
    `sync_queue` locks every row it reads.
  - *The leader lock is the exception.* A named lock belongs to one session, so its statements run on the lease's
    dedicated connection and are bound client-side (`interpolateParams`, safe with the utf8mb4 charset).
  - *Costs.* Server memory per prepared statement and connection: 7 statements × open connections × pods must stay
    well below `max_prepared_stmt_count` (16382 by default). Requires MySQL 8.0.19+, which the `INSERT ... AS new` row
    alias already did. Proxies that pool server sessions (ProxySQL, RDS Proxy) must support prepared statements.
- **Timezones.** `DATETIME` columns carry no zone. We assume UTC (configurable per pool) and always write `UTC_TIMESTAMP()`.
- **One transaction helper.** Every multi-statement write goes through `database.WithTx`: commit on success, rollback on
  error or panic, and bounded, jittered retries of the *whole* unit of work on deadlock (1213) or lock wait timeout (1205).
  There is no partially applied state.
- **Start-up.** The service waits up to `database.startup_timeout` for its pools before opening listeners, then exits
  non-zero, which makes a misconfiguration visible as a crash loop instead of a pod that is silently never ready.

### Partner API: data gaps and how they are handled

The schema does not contain everything the response must expose. Each gap below is handled explicitly and would be
confirmed with the business before going live.

| Requirement | What the schema offers | Decision |
|---|---|---|
| Acme's 4 statuses | 15 internal statuses | Mapping below, by `STA_iID` (stable) rather than by name. |
| Settlement timestamp | No such column. `TRX_dBooked` means "accepted for processing", not settled | `settled_at` = `TRX_dChanged` for `settled`/`settled_late` only (the last state change was the settlement), otherwise `null`. For reversed/chargeback the settlement time is lost. |
| Failure reason | No such column | Derived from the status: `declined_by_provider`, `declined`, `limit_exceeded`, `cancelled_by_customer`, `cancelled_by_operator`, `timed_out`. |
| Currency of a transaction | Transactions have no currency | The customer's **current** currency. If a customer's currency ever changed, older amounts would be mislabelled. Question: can it change? |
| Amount in major units | Minor units; `CUR_iMinorUnits` is `NULL` for USD | DB value, else ISO 4217 table in code (USD = 2), else `amount: null` plus a warning. Never guess (a wrong guess is a 100× error). Rendered with integer arithmetic as an exact JSON number. |
| Amount | `TRX_iAmount` is nullable; sign convention for payouts/adjustments unknown | `null` stays `null`; values are returned as stored. Question: are payouts negative? |
| Timestamps in RFC 3339 | `DATETIME` without zone | Assumed UTC (configurable per pool), emitted with `Z`. |
| Customer by email | Unique per brand only | Lookup within the token's brands; several matches → `409 customer_ambiguous` with the brand ids, repeat with `brand_id` (see below). |
| "Recent" transactions | Undefined | Newest first, page size default 20 / max 100 (config), keyset pagination for more. No date window. Question: is there a retention or time window Acme expects? |

**Status mapping** (`internal/domain/status.go`, unit-tested):

| Acme status | Internal statuses | Note |
|---|---|---|
| `pending` | reserved, pending_provider, awaiting_approval, approved, booked | approved/booked: accepted but no money moved yet |
| `completed` | settled, settled_late, **reversed, chargeback** | money did move; reversals are visible through `status_detail` |
| `failed` | declined_provider, declined_risk, declined_limit | risk declines are exposed as a generic `declined` so fraud rules are not disclosed to a third party |
| `cancelled` | cancelled_by_customer, cancelled_by_operator, cancelled_timeout | |
| `pending` | any unknown status id | logged as a warning; never claims money moved or a final state we cannot verify |

**Known limitation: unknown statuses and the `pending` filter.** A status id missing from the mapping (one added to
`transaction_statuses` later) is shown as `pending` with `status_detail: unknown`, but `status=pending` filters by the
*known* pending ids, so such a transaction is missing from a filtered answer. Including it would mean filtering by
"everything except the known non-pending ids", which also changes the cursor fingerprint, for a case that should not
happen in a stable mapping. Instead, every unknown id is logged as a warning and must be added to `statusMap`; new
statuses are a change to confirm with the business anyway.

Reversed and chargeback are the most debatable rows. Mapping them to `failed` or `cancelled` would contradict Acme's
definitions ("will not complete", "stopped before completion"), because the payment *did* complete. `completed` plus
`status_detail: reversed/chargeback` keeps the vocabulary correct and lets the support agent see what happened.
Questions: is this mapping acceptable? Should reversals rather be reported as separate events?

**Same email in several brands.** A token may cover several brands, while emails are unique per brand only. We return
`409 customer_ambiguous` with `details.brand_ids` and let the caller repeat the request with `brand_id`. Alternatives:
return all matching customers with their transactions (a bigger, less predictable response, and one support ticket
usually concerns one brand), or always require `brand_id` (more friction for the common single-match case). The chosen
behaviour is explicit, cheap and never mixes customers in one answer.

**Pagination.** Keyset pagination on `(TRX_dCreated DESC, TRX_iID DESC)`: stable while new transactions arrive, ties on
the second-precision timestamp broken by id, and every page is an index range scan on the existing
`(TRX_CUS_iID, TRX_dCreated)` key (InnoDB appends the primary key). Offset pagination would skip or repeat rows and
cost O(offset). The cursor is opaque and bound to the customer and status filter, so it cannot be replayed against a
different query.

**Query plan, measured.** The first version joined `transaction_types`/`payment_methods` directly. MySQL hash-joins
those tiny tables, which discards the index order and turns every page into a sort of the customer's *entire*
history (`Using temporary; Using filesort`, 200k rows read, ~0.9s per page for a customer with 200k transactions). The
query now selects the page from `transactions` alone in a derived table (`Backward index scan`, stops after `limit`
rows) and joins the names onto those rows only: p50 16 ms for the same customer. An integration test reads MySQL's
handler counters and fails if a page reads more than a small multiple of its size.

**Email matching.** The column collation `utf8mb4_0900_ai_ci` is accent-insensitive as well as case-insensitive, so
`jané.doe@example.com` would have returned `jane.doe@example.com`'s transactions. The indexed comparison narrows the
candidates and an exact, case-insensitive comparison in Go decides. Forcing an accent-sensitive collation in SQL would
have disabled the index.

**Cursor integrity.** Cursors are opaque but not signed. A partner editing one can only move within the same customer
and filter it is already authorized for, so we did not add an HMAC; it would be a small addition if cursors were ever
to carry authorization-relevant data.

**Privacy and safety.**
- Customers outside the token's brands answer `404`, never `403`, so a partner cannot probe which emails exist elsewhere.
- Emails are masked in logs, and the query string (which carries the email) is never logged. Proxies in front of the
  service may still log URLs; a `POST /v1/transactions/search` with the email in the body would avoid that, at the cost of
  a non-idiomatic read endpoint. Question for Acme: can they send the email in a body?
- IDs are strings (64-bit ids exceed JavaScript's safe integer range); amounts are exact decimal literals.
- Inactive customers are still returned, since support needs their history. Question: confirm.

### Contact sync

**Shape.**
- `sync_queue` behaves like a transactional outbox written by the producers of customer changes, and the sync is its
  relay (the "polling publisher" variant).
- It runs as a goroutine in every instance. A MySQL named lock (`GET_LOCK`) elects the single replica that syncs in a
  given cycle, because Acme's rate limit is **per API key**: several replicas syncing at once would share each brand's
  60 requests/minute without knowing it.
- One query fetches the oldest `batch_per_brand` outstanding entries **per brand** (window function). Entries are
  coalesced per customer, and each brand is processed by its own worker pool against its own token bucket. Answers may
  take 30s, so one worker could not use the budget.

**Delivery semantics: at-least-once with idempotent effects, so the result is effectively-once.**
- *Create* is keyed by `external_id` = our customer id. Acme rejects a second contact with `409`, which we resolve by
  looking the contact up by email and updating it (outcome `recovered`). This covers crashes and timeouts after Acme
  created the contact but before we stored its id.
- *Update* sends the customer's full current snapshot (read after the queue entries were fetched), so sending it twice
  is harmless.
- *Recording* happens in one database transaction (`WithTx`): upsert `partner_contacts`, then mark **exactly the fetched**
  entries processed (`AND SYQ_dProcessed IS NULL`). An entry queued while the customer was being synced stays
  outstanding and triggers another, newer push.
- **Consumer-side dedup.** Retries, re-queued entries and the short overlap of two leaders after a lost lock can send
  the same PATCH more than once. Acme must tolerate repeated updates, which it should since they are idempotent. If Acme
  supports an `Idempotency-Key` header we would send `sha256(customer id + payload)`. Question for Acme.
- Possible improvement: remember the hash of the last payload synced per customer and skip PATCHes that change nothing,
  which would save rate budget. There is no column to persist it, so it would live in memory or in a service-owned table.

**Never drop an entry (decision).** An entry is marked processed only after a confirmed sync. The schema gives us no
status, attempts or error columns, and we may not write anything else, so failures are tracked like this:
- customer-specific failures (Acme rejected the data, a 409 we cannot resolve, a deleted customer) back off exponentially
  in memory (1 min, doubling, up to 1 h), and those customers are excluded from the fetch so they do not block the
  batch;
- partner failures (5xx, timeouts, an open circuit, a refused API key) and database failures stop that brand for the
  current cycle without blaming the customers; the next cycle retries;
- orphan entries (customer row missing) and entries of brands without credentials are reported every cycle and kept.

The cost is that some entries stay outstanding until somebody acts. What we would add with a schema change: a migration
adding `SYQ_cStatus`, `SYQ_iAttempts`, `SYQ_cLastError` and `SYQ_dNextAttempt` (plus an index on
`(SYQ_dProcessed, SYQ_dNextAttempt)`). That makes backoff durable across restarts and leader changes, lets the fetch
filter on it, and enables a small admin tool or service to list parked entries and let an operator retry, fix or close
them after confirming with the people involved. A dead-letter queue would play the same role in a broker-based design.

**Investigating a sync: logs are the audit trail.** We may not write anything but the two tables, so the record of
every sync outcome is the structured log, not a database table:
- one line per customer and attempt (`customer synced` / `customer sync failed`) with `run_id`, `brand_id`,
  `customer_id`, `queue_ids`, `outcome`, `contact_id`, duration, queue lag, the error and the next retry time;
- one line per cycle (`sync cycle finished`) with the counts per outcome, orphan and unconfigured entries;
- durable timestamps of the last success in the database: `partner_contacts.PCO_dSynced` and `sync_queue.SYQ_dProcessed`;
- metrics for trends and alerts, and `GET /internal/sync/status` for the last cycle (in memory, per replica).

The logs must therefore be shipped to central log storage (Loki, ELK, CloudWatch) with a retention that matches how
far back support or audits need to look. Searching by `customer_id` or `run_id` then answers "what happened to this
customer?" across replicas and restarts. With a service-owned table (see "What remains") the same lines could also be
written as rows.

**Failure catalogue (as implemented).**

| Failure | Handling | What remains |
|---|---|---|
| DB unreachable at the start of a cycle | the cycle fails, is logged and retried after `interval` | sync delayed |
| Leader's lock connection drops | `Lease.Held` (checked before every job, at most every `lock_check_interval`) fails, the leader stops dispatching, another replica takes over | a short overlap of two leaders: idempotent, costs a few extra calls |
| Leader crashes or is SIGKILLed | MySQL frees the lock with the session; `wait_timeout` on the lock session bounds a zombie session after a partition | in-flight entries stay outstanding and are retried |
| Lock session idle while every worker is inside a slow job | the lock connection carries no traffic during jobs, so `lock_session_timeout` (its `wait_timeout`, 5 min) must exceed `job_timeout` + `lock_check_interval`; the config refuses anything shorter. Otherwise MySQL would drop the session and free the lock mid-cycle, letting a second replica start | a vanished leader's lock is held up to 5 min, delaying the sync by as much |
| Acme timeout (> 30s), result unknown | retried; a create that did happen comes back as 409 and is recovered | unresolvable if the email changed meanwhile (see questions) |
| Acme 5xx or network error | retried with jittered exponential backoff; the per-brand circuit breaker opens after 5 consecutive failed calls; the brand stops for the cycle | backlog grows while Acme is down |
| 429 | the brand's token bucket pauses until `Retry-After` (seconds or HTTP date; 60s default), then resumes at the steady rate | none |
| 401/403 (key revoked or rotated) | the brand pauses for `auth_cooldown`, error log | needs a person |
| 422 / other 4xx | the customer backs off; the entry stays | until the data changes or someone acts |
| 409 and the email lookup finds nothing | as 422 (`conflict_unresolved`) | needs Acme support (see questions). Observed in the crash test: a customer **without email** created right before a SIGKILL cannot be resolved (1 of about 150); customers with an email recovered (7 of 7) |
| 404 on update (contact deleted in Acme) | created again; the new id is stored | none |
| 2xx without an id | treated as unavailable; the retry resolves it through 409 | none |
| DB write fails after Acme succeeded | the transaction rolls back (no half state); the next attempt recovers through 409 | extra calls |
| Deadlock or lock wait timeout while recording | `WithTx` retries the whole transaction | none |
| SIGTERM during a cycle | no new job starts; jobs in flight finish their call and write (`WithoutCancel`, bounded by `job_timeout`); shutdown waits for them | a hard kill is the crash case |
| Backlog bigger than the budget | the next cycle starts immediately while batches are full *and* progress is made (no hot loop when Acme is down) | throughput is capped by Acme's limit |

**Throughput.** 55 requests/minute per brand (kept below 60 so other clients can share the key) means roughly 50
contacts per minute per brand. An initial backfill of 100k customers would take about 33 hours. Question: does Acme
offer a bulk import for the first load?

**Sizing the workers.** The budget is only reachable with enough concurrent calls: workers needed ≈ request rate ×
answer time (Little's law). The default `workers_per_brand: 4` saturates 55/min while Acme answers within about 4s.
If Acme really answers in its documented worst case of 30s, 4 workers make only about 8 calls per minute and the
backlog drains about 7 times slower; ~28 workers would be needed. Each worker is a goroutine waiting on HTTP, so
raising it is cheap. The Acme latency metric (`vyking_acme_request_duration_seconds`) shows when to tune it.

**Questions for Acme Support.**
- Can a `409` return the existing contact id, or can contacts be looked up by `external_id`? Today a contact created
  before a crash cannot be found if the customer's email changed in between.
- What is the lookup response shape, and the answer when nothing matches? We assume `{"id": ...}` or `404`.
- Is PATCH a full replace or a partial update, and how is a field cleared? We always send the full contact and omit
  empty fields.
- Are emails unique per account? Agents may create contacts manually; `sync.lookup_before_create` adopts them at the
  cost of one extra call per new customer.
- Is there a DELETE endpoint? Without one, a customer's GDPR erasure cannot be propagated, and a customer moving to
  another brand leaves a contact behind in the old brand's account.
- Which phone and country formats are accepted? The schema does not validate them, so 422s are likely.

**Database cost.** The fetch filters on `SYQ_dProcessed IS NULL`, which has no index, so it scans `sync_queue`, a table
that only grows. Recommended for the schema owners: an index on `(SYQ_dProcessed, SYQ_iID)` and a retention job for
processed rows. Everything else is primary-key access.

### Cache for partner lookups

- Support agents tend to repeat the same lookup while handling a ticket, so successful results are cached for a short
  TTL (15s, configurable; `CACHE_ENABLED=false` turns it off). Errors are never cached, so a customer created a moment
  ago is found immediately.
- **The key contains the effective brand scope** (plus email, statuses, page size and cursor), so a result is never
  served to a caller with different entitlements.
- **Staleness is the trade-off.** Writes happen in other systems, so the cache cannot be invalidated and the TTL alone
  bounds staleness; a transaction may show its previous status for up to 15s. A CDC stream (see "Load and scalability")
  could invalidate keys on change.
- **Swappable backend.** The use case depends on a two-method `ResultCache` port. The in-process `Memory` cache is bounded
  (max entries, periodic sweep), costs nothing to operate and is a readiness dependency like any backend would be. It is
  per replica, so hit rates drop as pods are added and each pod holds its own copy of the PII. A Redis implementation
  would be one more type in `internal/adapter/cache` (serialize `Result`, `PING` for readiness) with no change to the
  service or handlers. It would then need encryption in transit, access control and short TTLs, since it stores
  personal data.

### Partner authentication (deliberately unspecified by the task)

The task only says Acme Support calls us with `Authorization: Bearer <partner-token>` and that a partner must only access
the brands it is authorized for. It says nothing about the token format, who issues it, its lifetime, how it is revoked
or how Acme obtains it. This is the least specified part of the integration, so the decision is isolated: handlers depend
on a one-method `TokenVerifier` interface and the scheme can be replaced without touching them.

**Assumption.** We issue each partner a signed JWT out of band (it is configured once in Acme Support). The token carries
the partner's entitlements, so every replica verifies it statelessly: no session store and no database lookup per request.

```
header:  {"alg": "HS256", "typ": "JWT", "kid": "2026-11"}
payload: {"iss": "vyking-integration", "aud": "vyking-partner-api", "sub": "acme-support",
          "brands": [1, 2], "scope": "transactions:read", "iat": …, "nbf": …, "exp": …, "jti": "…"}
```

- **Verification.** Signature, `kid`, algorithm (pinned to HS256, so `alg: none` and RS/HS confusion are rejected),
  issuer, audience, `exp` (required), `nbf`, `iat`, 30s clock-skew leeway, non-empty `sub` and `brands`. A valid token
  without the `transactions:read` scope is rejected with 403.
- **Library.** `golang-jwt/jwt/v5` (no transitive dependencies) instead of hand-written JWS parsing; this is
  security-sensitive code where a vetted implementation beats saving one dependency.
- **Key rotation.** Several keys can be active (`auth.keys`, selected by `kid`): add the new key, re-issue tokens, remove
  the old key. Secrets come from env/secret store and must be ≥ 32 bytes (RFC 7518).
- **Errors** follow RFC 6750 (`WWW-Authenticate` challenges) with stable codes: `unauthorized` (no/foreign scheme),
  `token_malformed`, `token_expired`, `token_invalid` (signature, issuer, audience, `nbf`, unknown `kid`, no brands) and
  `insufficient_scope`. Reasons are logged; tokens never are.
- **Brand scoping.** The `brands` claim is the authorization boundary. Customers of other brands are reported as
  `404 customer_not_found`, not 403, so a partner cannot probe which emails exist elsewhere.
- **Test tokens.** `make tokens` prints valid, expired, not-yet-valid, forged, unknown-kid, wrong-audience/issuer,
  no-brands, no-scope, `alg: none` and malformed tokens signed with the local config; a test guarantees each one produces
  the outcome its name promises.

**Trade-offs and weaknesses.**
- A JWT cannot be revoked before it expires. Mitigations: short lifetimes plus rotation, or a `jti` deny-list (shared
  cache) if Acme needs long-lived tokens.
- HS256 is symmetric: whoever can verify can also mint. That is acceptable while we are both issuer and verifier, but
  not if tokens are issued by another system.

**Alternatives we would discuss with Acme Support and Security.**
1. *Opaque API keys*: a random key per partner, stored hashed (SHA-256) with its brand mapping. Simplest for a static
   header and instantly revocable; needs a lookup per request (cacheable).
2. *OAuth 2.0 client credentials*: an identity provider issues short-lived RS256/EdDSA tokens and we verify against its
   JWKS. Best fit for several partners, with rotation and revocation handled by the IdP. Our verifier would only change
   its key source.
3. *mTLS* per partner, possibly combined with one of the above.

**Questions.** How can Acme Support send credentials (static header only, or OAuth)? One token per brand or one token
covering several brands? Acceptable lifetime and rotation process? Should inbound requests be rate-limited per partner?

### Observability

- **Business metrics, not just HTTP.** Besides request rate, latency and errors per route, we count:
  - partner lookups by outcome and rejected tokens by code;
  - cache hits;
  - Acme answers, latency and client events (throttled, circuit opened, key refused) per brand;
  - sync outcomes per brand and customer.

  The single most useful sync signal is the **age of the oldest outstanding entry per brand**: if it keeps growing the
  backlog is not draining, whatever the cause.
- **Prometheus stays in one package.** Services and adapters report through small observer interfaces (nop by
  default); only `internal/platform/metrics` imports Prometheus, on a private registry.
- **Bounded cardinality.** Labels are route patterns (an unmatched path becomes `unmatched`, never the raw URL),
  configured brand ids and fixed outcome or code vocabularies. No emails, customer ids or tokens appear in labels.
- **Multiple replicas.** Only the leader describes the queue. A replica that loses leadership clears its queue gauges,
  so dashboards (`max by (brand)`) do not show stale ages from an earlier leader.
- **Alerts we would start with:**
  - oldest outstanding age above 30 minutes for a brand;
  - any `key_refused` event;
  - `circuit_opened` repeatedly within 15 minutes;
  - partner API 5xx ratio above 1%;
  - p95 latency above 500 ms;
  - `/ready` failing on every pod.

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

## Load and scalability

The task gives no volumes. These are the questions we would ask first, followed by how the design responds as load grows.

**Questions.** How many brands, and how many customers per brand? What is the peak `sync_queue` inflow (entries per
second)? What partner API QPS and latency SLO does Acme expect? How fast is `transactions` growing?

**Writes by this service are small.** Every synced customer costs one short transaction (an upsert plus one
primary-key UPDATE per coalesced queue entry, usually one).
Acme's rate limit caps that at about one transaction per second per brand, so 50 brands mean about 50 transactions per
second. MySQL is not the bottleneck here; Acme's limit is. The real write volume belongs to the producers (customer
changes, transactions). Processed `sync_queue` rows are never deleted and need a retention or archiving job;
partitioning by `SYQ_dQueued` would be the schema owners' call.

**Reads (partner API).** Each request is one email lookup on an index and one bounded index range scan (see "Query plan,
measured"), and the cache absorbs the repeat lookups typical of ticket handling.
- Reads go to a **read replica** with its own connection string and pool (`database.replica`). More replicas can sit
  behind a reader endpoint or ProxySQL as read demand grows. A few seconds of replication lag is fine for support use.
- The sync **never** reads from a replica (see "Database access").
- A shared cache (Redis) would raise hit rates across pods; the port is ready for it.

**Splitting the service when metrics show degradation.** One deployable currently holds two different workloads:
- the partner API, which is latency-sensitive, scales out with read QPS and is light on CPU and IO;
- the sync, a long-running worker bound by Acme's rate limit and latency, which needs exactly one leader.

If they start to interfere, they become two deployments built from the same image: `PUBLIC_ENABLED=false` runs a
sync-only worker and `SYNC_ENABLED=false` an API-only one. The signals to watch are:
- API p99 latency or error rate rising during sync cycles;
- CPU, memory or GC pressure (pprof, Go runtime metrics);
- the two workloads needing different replica counts.

The hexagonal layout makes the split a wiring change. A second `cmd/` entry point would only be cosmetic.

**OLTP vs OLAP: keep heavy reads off the production OLTP database.** The primary carries critical writes (payments,
customer changes). Serving a growing partner or analytics read load from it is not good practice, and neither is
serving it from replicas of the same cluster: long scans, buffer-pool churn and replica lag all compete with the write
path. How it would evolve:
1. Now: read replica plus cache for the partner API's point lookups.
2. Higher demand: **export changes with CDC** (Debezium reading the binlog into Kafka), feeding one or both of:
   - a purpose-built read model for the partner API (a CQRS projection keyed by brand and email, with status mapping,
     currency and amounts precomputed);
   - a data warehouse or OLAP store (ClickHouse, BigQuery, Snowflake) for high-volume querying, reporting, BI dashboards
     and sync auditing.
3. The trade-offs are freshness within seconds (CDC lag, acceptable for support), new infrastructure, and data
   governance: copies of PII, retention, and GDPR erasure that must also reach the warehouse.

The same CDC stream can replace the polling relay: `sync_queue` inserts flow into a topic keyed by customer id, per-brand
consumers sync at their rate limit, and a dead-letter topic takes over from the missing status column. That also removes
the full scan on `SYQ_dProcessed`.

**Sharding, only if one primary cannot keep up.**
- Shard by **brand**, the natural tenant boundary: credentials, rate limits, partner scope and email uniqueness are all
  per brand, and every query is brand-scoped already.
- Brand sizes are very uneven, which risks hot spots. Use directory (lookup) sharding, so a big brand can get its own
  shard, and hash `customer_id` *within* oversized brands. Per-customer queries stay on a single shard.
- Never shard by time or by sequential id ranges: the newest shard would take all the writes.
- Email lookups route by brand; multi-brand tokens fan out to a few shards.

## Recommended indexes (for the schema owners; we did not change the schema)

| Index | Why |
|---|---|
| `sync_queue (SYQ_dProcessed, SYQ_iID)` | the sync's fetch of outstanding entries currently scans the whole, ever-growing table |
| `transactions (TRX_CUS_iID, TRX_STA_iID, TRX_dCreated)` | only for customers with long histories that are mostly filtered out by status; costs write amplification on a hot table, so measure first |
| drop `customers KEY (CUS_BRD_iID)` | redundant: it is the prefix of `UNIQUE (CUS_BRD_iID, CUS_cEmail)` |

The existing `(TRX_CUS_iID, TRX_dCreated)` and `(CUS_BRD_iID, CUS_cEmail)` / `CUS_cEmail` keys serve the partner API
well.

## What remains and what we would improve with more time

- Durable sync state (status, attempts, last error columns, or a service-owned table) plus an admin tool for parked
  entries, replacing the in-memory backoff.
- Inbound rate limiting and load shedding per partner on the public API.
- Signed (HMAC) cursors if they ever carry authorization-relevant data.
- OpenTelemetry tracing across the API, the database and Acme calls (request ids already propagate as `X-Request-ID`).
- Kubernetes manifests (Deployment, PodDisruptionBudget, ClusterIP service for the internal port, NetworkPolicy), and
  config delivered as ConfigMap plus Secret.
- Defense in depth for the internal port. Today only network isolation keeps `/internal/sync/run` and `/metrics`
  private. A NetworkPolicy that admits only Prometheus and the kubelet probes would enforce it, and a shared secret
  (or mTLS) on the sync trigger would protect it if a policy were ever misconfigured.
- Contract tests against Acme's real sandbox once available; today the mock encodes our reading of the contract.
