# load/ — k6 load tests (client API)

Capacity and SLO testing for the bank0 client API. **Ready to run; not yet executed**
against a deployed target (needs k6 + a running `mode=api` binary + a tuned Postgres).
These exist so capacity is a measured number before launch, not a guess.

## Run

```bash
# install: https://grafana.com/docs/k6/latest/set-up/install-k6/
k6 run -e BASE_URL=http://localhost:8090 -e USER=alice -e PASS=password load/transfers.js
```

Test the **deployed shape** — the compiled binary in `mode=api` behind realistic
resources, Cloudflare in front — not `mode=all` and not `httptest`. Reset to a known
seeded state before each stage (`task seed:demo`).

## The correctness oracle

After every run, the ledger must still hold. This matters more than any latency number:

```sql
SELECT * FROM reconcile();   -- must return zero rows
```

A load run that posts a deadlock-free, fast, but unbalanced ledger has found the worst
possible bug. Snapshot `pg_stat_statements` / `pg_stat_database` (deadlocks must stay 0)
deltas per stage.

## Stages (the campaign)

`transfers.js` implements **Stage 1 (baseline)** — alternating-direction transfers at a
ramping arrival rate, with p99 thresholds as regression gates. The remaining stages are
variants of the same script:

1. **Baseline** (here): unique account pairs / alternating direction → p50/p95/p99 + max
   sustained TPS at the default pool. Set the SLOs from this run, then enforce.
2. **Contention**: drive the known hot rows — many VUs debiting ONE shared account, and
   concurrent **same-Idempotency-Key** bursts (swap `uuidv4()` for a shared key) → measure
   lock-wait queuing; assert exactly one ledger effect per key (no double-debit), `in_progress`
   maps to 409. The single `EXTERNAL_CLEARING` account (every deposit/withdraw touches it)
   is the predicted global write bottleneck.
3. **Mixed soak**: read-heavy steady load for 30–60 min → pool exhaustion, connection
   leaks, maintenance-loop interference.

## What to watch (DB-side)

- **Balance-cache trigger** serializes writes per account row — the shared/clearing account
  is the first bottleneck.
- **Lock ordering**: concurrent A→B and B→A must not deadlock (the deterministic
  lowest-id-first `FOR UPDATE` is also covered by `internal/db/concurrency_test.go`).
- **Pool sizing**: `max_open_conns` (default 10) × replicas must stay well under Postgres
  `max_connections`; with `FOR UPDATE` serialization, more app conns than the DB can run
  just deepens lock queues. Watch pool-acquire waits and confirm `conn_timeout` yields a
  clean 503, not a pile-up.
