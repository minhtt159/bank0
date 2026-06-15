# CLAUDE.md — working in this repo

bank0 is a **core-banking backend**: a double-entry ledger where correctness is a
property of the database, fronted by a thin Go API. Read this before editing; it
encodes the conventions that keep the code coherent. Deep detail is in
[`docs/`](docs/) (start at [`docs/01-overview.md`](docs/01-overview.md)).

## The five rules (don't violate without saying so)

1. **Logic lives in the database.** Every money movement and every auth/session
   transition is a PL/pgSQL function with row locks. Go handlers parse the request,
   call **one** DB function, and map the result/error to HTTP. No balance math, no
   "is this allowed?" in Go.
2. **The ledger is the source of truth.** `accounts.balance_minor` is a
   trigger-maintained cache of `SUM(ledger_entries)`. Nothing does
   `UPDATE accounts SET balance = …`. Corrections are new reversing entries
   (append-only; an `UPDATE`/`DELETE` on `ledger_entries` is rejected by trigger).
3. **Contract-first.** `api/openapi.yaml` is the source of truth for the HTTP API;
   `oapi-codegen` generates the server interfaces and drift is a **build error**.
   Edit the spec → regenerate → implement → `go build`.
4. **Idempotency & money.** Money moves carry an `Idempotency-Key`; replays return
   the original result. All amounts are **int64 minor units** — never floats.
5. **`mapDBError` is the only place HTTP status meets business meaning.** It
   translates SQLSTATEs raised by DB functions into status codes. Add a case there;
   don't scatter business checks into handlers.

## Three surfaces (one Go binary + one Worker)

| Host | Surface | How | Doc |
|---|---|---|---|
| `portal.bank0.hnimn.art` | admin API + operator console (HTML) | Go `mode=portal`, cookie sessions | [`docs/05`](docs/05-admin-ui.md) |
| `api.bank0.hnimn.art` | client JSON API | Go `mode=api`, JWT + refresh, Cloudflare-fronted | [`docs/06`](docs/06-client-api.md) |
| `bank0.hnimn.art` | customer PWA | Cloudflare Worker (Preact/Vite), proxies `/api/*` | [`docs/07`](docs/07-client-web-app.md) |

`mode=all` serves both Go surfaces locally. Always-public on every surface:
`/health` (DB-blind liveness), `/readyz` (DB-aware readiness), `/metrics`,
`/openapi.yaml`, `/docs`. The client public auth routes
(`/auth/login,/refresh,/logout`) are registered on the **parent** router ahead of
the JWT-guarded subrouter so they aren't shadowed; in `all` mode the one admin
route that collides with the client's `/transfers/{id}` — `GET /transfers/pending`
— is registered first behind the session guard.

## Where things live

```
api/openapi.yaml            HTTP contract (client+admin tags) -> genclient/genadmin
db/migrations/*.sql         goose migrations (schema + ALL PL/pgSQL functions)
db/queries/*.sql            sqlc queries  -> internal/db/sqlc/*.gen.go
internal/db/bank.go         hand-written pgx for set-returning fns sqlc can't expand
internal/db/auth.go         sessions + refresh-token DB calls (hand-written pgx)
internal/api/handlers_*.go  thin HTTP handlers (client + admin)
internal/api/console*.go    operator console (portal) handlers + routes
internal/api/respond.go     writeJSON / writeError / mapDBError
web/template/*.templ        operator console UI (Templ + HTMX)  -> *_templ.go
web/app/                    customer PWA (Preact + Vite + TS)
worker/                     Cloudflare Worker (static host + /api proxy)
```

## Build · run · test · generate

Use the Taskfile (`task --list`). Key targets:

```bash
task generate        # sqlc -> oapi-codegen -> templ  (after editing spec/queries/migrations/templ)
task build           # generate + go build
task test            # go test -race ./...   (DB integration tests SKIP without a DSN)
task test:db         # spin up Postgres 18 + run DB integration tests
task run             # run the binary (needs Postgres + migrations)
task webapp:build    # typecheck + Vite build the PWA
```

**Pinned generator versions** (match these or the committed generated files churn):

```bash
go install github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1
go install github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.7.1
go install github.com/a-h/templ/cmd/templ@v0.3.1020
```

After regenerating, **commit the generated files** (`internal/db/sqlc/*`,
`internal/api/gen*/*.gen.go`, `web/template/*_templ.go`) — the repo builds without
the tools installed.

## Testing against PostgreSQL (default 18; 17 for Supabase parity)

The integration tests (DB + HTTP) are **DSN-gated**: they skip unless
`TEST_DATABASE_DSN` is set, and `TestMain` migrates the target DB fresh. **Postgres
18 is the default** everywhere (local dev + CI). Postgres 17 (the Supabase deploy
target) is supported as an **opt-in** via the `uuidv7()` polyfill in migration
`00001` (a no-op on PG18+, where the built-in wins). Don't go below 17.

```bash
# preferred: Taskfile (deploy/docker-compose.dev.yml, postgres:18)
task test:db            # Postgres 18 (default)
task test:db PG=17      # Postgres 17 — Supabase parity (recreates a fresh db container)

# manual, full suite:
export TEST_DATABASE_DSN='postgres://admin:admin@localhost:5432/bank0_test?sslmode=disable'
go test -count=1 ./internal/db/ ./internal/api/
```

CI defaults to `postgres:18`; trigger the workflow manually (Actions →
**workflow_dispatch** → `pg_version: 17`) to run the suite against PG17.

If Docker Hub is rate-limited in your environment, pull PG18 from the GCR mirror:
`docker run -d --name pg18 -e POSTGRES_USER=admin -e POSTGRES_PASSWORD=admin -e POSTGRES_DB=bank0_test -p 5544:5432 mirror.gcr.io/library/postgres:18-alpine`,
then point `TEST_DATABASE_DSN` at `:5544`. Always run **migrate up → down → up** on
a throwaway DB to confirm a new migration is reversible.

## Common tasks (the patterns to copy)

- **New client endpoint:** add the op to `api/openapi.yaml` (tag `client`) →
  `task generate:oapi` → implement the method on `*Server` (it won't build until you
  do) → scope to the subject with `clientSubject(r.Context())`. Keep ops that need
  query/body params **client-only**: an op shared by both tags must be path-param
  only, else the two generated packages produce conflicting `Params` types.
- **New DB logic:** write the PL/pgSQL in a new `db/migrations/NNNN_*.sql` (with a
  working `-- +goose Down`); add a query in `db/queries/*.sql` and
  `task generate:sqlc`. **sqlc cannot expand set-returning functions**
  (`RETURNS TABLE`) — hand-write those with pgx in `internal/db/bank.go` or
  `auth.go` (see `Transfer`, `ResolveAccountByIban`, `RotateRefreshToken`).
- **Raising inside a function that must persist a side effect:** a PL/pgSQL `RAISE`
  rolls back that function's own writes. If you need a write to survive the error
  (e.g. refresh-token reuse revoking the family), do the write in a **separate
  statement from Go** after catching the SQLSTATE — see `RotateRefreshToken` +
  `revoke_refresh_family`.
- **Console action:** handler in `console_handlers.go` (gate with
  `s.requireRole`), route in `console.go`, button in the relevant `*.templ`, then
  `task generate:templ`. Mutations set `HX-Trigger: bank0:refresh` and re-render.

## Before you finish

`go build ./... && go vet ./...`, run the suite against PG18, and rebuild the PWA
(`task webapp:build`) if you touched `web/app/`. Commit generated code alongside
its source.
