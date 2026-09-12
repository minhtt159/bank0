# bank0 - Development

**TL;DR.** Bring up the stack with compose, run `task generate` after touching
the spec, the queries or a template, and commit the generated files. The
database is the program: a change to money or auth logic is a new migration, not
a new `if` in Go. Integration tests skip silently unless `TEST_DATABASE_DSN`
points at a Postgres 18.

This is the guide for working *on* bank0. To run it, read the
[README](../README.md); to understand why it is built this way, read
[`01-overview.md`](01-overview.md). `CLAUDE.md` at the repository root is the
condensed version of this document for agents.

Prerequisites: Go, Docker, SQL, and `task`
([Taskfile](https://taskfile.dev)). `task --list` shows everything available.
Node is needed for two targets only: `task lint:openapi` and anything under
`web/app/`.

---

## 1. The local stack

```bash
task install                  # the pinned generators (sqlc, oapi-codegen, templ)
task dev:reset                # clean DB, migrate, seed, bring both surfaces up
task test                     # go test -race ./... (DB tests skip without a DSN)
task test:db                  # start Postgres 18 and run the DB-backed tests
```

`task dev:reset` is the one to reach for when the database has drifted into a
confusing state; it rebuilds from empty and re-seeds in a single step.

The generators are version-pinned. Matching those versions matters, because a
different version regenerates the committed files differently and the diff shows
up in CI:

```bash
go install github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1
go install github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.7.1
go install github.com/a-h/templ/cmd/templ@v0.3.1020
```

---

## 2. Generated code is committed

The repository builds without any generator installed, which means every
generated file is checked in and must be regenerated and committed alongside its
source.

| You changed | Run | Commit |
|---|---|---|
| `db/queries/*.sql` or a migration | `task generate:sqlc` | `internal/db/sqlc/*` |
| `api/openapi.yaml` | `task generate:oapi` | `internal/api/gen*/*.gen.go` |
| `web/template/*.templ` | `task generate:templ` | `web/template/*_templ.go` |
| any of the above | `task generate` | all of it |

CI runs the same generators and fails if the result differs from what you
committed.

---

## 3. The API contract comes first

`api/openapi.yaml` is the source of truth for the HTTP API. `oapi-codegen`
generates one Go `ServerInterface` per surface, filtered by tag, and `*Server`
implements both:

```
api/openapi.yaml --oapi-codegen--> internal/api/genclient (tag: client)
                 \--------------->  internal/api/genadmin  (tag: admin)

internal/api/server.go:  var _ genclient.ServerInterface = (*Server)(nil)
                         var _ genadmin.ServerInterface  = (*Server)(nil)
```

Those two compile-time assertions are what make spec drift a *build error*. Add
an operation to the spec, regenerate, and the code will not compile until you
implement it. Change a signature in the spec and every handler that no longer
matches stops compiling.

```bash
# edit api/openapi.yaml, then:
task generate:oapi
task lint:openapi     # Spectral audit; needs node
go build ./...        # drift surfaces here
```

The spec is served at `/openapi.yaml` and rendered at `/docs` on every surface.

One constraint the code generator imposes: an operation carried by **both** tags
must take path parameters only. An operation with query or body parameters
generates a `Params` struct into each package, and the two definitions collide.
That is why `getAccountLedger` is client-only and the console reads the ledger
straight from the database instead.

---

## 4. Adding a client endpoint

1. Add the operation to `api/openapi.yaml` under the `client` tag.
2. `task generate:oapi`. The build breaks - that is the point.
3. Implement the method on `*Server` in `internal/api/handlers_*.go`.
4. Scope it to the caller with `clientSubject(r.Context())`, or
   `clientSubjectOr401` when the handler needs a subject to do anything at all.
   Reads of something the caller does not own are a 404 - the API does not
   confirm it exists. Writes *from* something they do not own (a debit account,
   a `from_account`) are a 403, because the resource is named in the request and
   hiding it would be dishonest rather than safe.
5. Keep the handler thin: parse, call one DB function, map the error. If you are
   writing an `if` about money or permissions in Go, it belongs in the database.

---

## 5. Adding database logic

The 17 domain migration files are **frozen** - they are the `v1.0.0` baseline.
Every change since, whether a schema change or a one-line fix inside a PL/pgSQL
function, goes in a new `db/migrations/NNNN_*.sql` with a reversible
`-- +goose Down`.

Never edit a frozen file. Goose will not re-run a migration on a database that
already applied it, so an edit is silently skipped on every existing
installation while a fresh install gets the new version - a green deploy whose
schema has quietly diverged. `TestMigrationsReversible` runs up, down and up
again on a throwaway database and fails if your `Down` does not restore what was
there.

Then add the query in `db/queries/*.sql` and `task generate:sqlc`.

Two things sqlc cannot do for you:

- **Set-returning functions.** sqlc cannot expand `RETURNS TABLE`. Hand-write
  those with pgx in `internal/db/bank.go` or `internal/db/auth.go` - see
  `ClientTransfer`, `ResolveAccountByIban`, `RotateRefreshToken`.
- **Changing a function's result columns.** `CREATE OR REPLACE FUNCTION` cannot
  alter a `RETURNS TABLE` signature (SQLSTATE 42P13). Drop and recreate the
  function inside the migration, and have the `Down` restore the previous body
  verbatim - `00019_login_returns_must_change_password.sql` is the worked
  example.

### The RAISE rollback trap

A PL/pgSQL `RAISE` rolls back that function's own writes. If a side effect must
survive the error - revoking a refresh-token family after detecting a replay,
recording a failed verification attempt - the write has to happen in a separate
statement issued from Go after catching the SQLSTATE. `RotateRefreshToken`
calling `revoke_refresh_family` is the pattern to copy.

### Mapping errors

`mapDBError` in `internal/api/respond.go` is the only place a SQLSTATE becomes an
HTTP status. Add a case there; never scatter the business check into a handler.
The current mapping is tabulated in
[`03-ledger-lifecycle-idempotency.md`](03-ledger-lifecycle-idempotency.md).

---

## 6. Adding a console action

1. Handler in `internal/api/console_handlers.go`, gated with `s.requireRole`.
2. Route in `internal/api/console.go`.
3. Button in the relevant `web/template/*.templ`.
4. `task generate:templ`.

Mutations set `HX-Trigger: bank0:refresh` and re-render the affected fragment
rather than the whole page. A session alone is not authorization: an admin-only
mutation needs its role gate explicitly.

---

## 7. Testing against Postgres 18

The integration tests are DSN-gated. They **skip** rather than fail when
`TEST_DATABASE_DSN` is unset, so a green `task test` does not by itself mean the
database-backed tests ran. `TestMain` migrates the target database from scratch.

Postgres 18 is the only supported version: the schema's `DEFAULT uuidv7()` uses
the built-in, and there is no polyfill for older servers.

```bash
task test:db      # compose up postgres:18 and run the DB tests

# or point at your own:
export TEST_DATABASE_DSN='postgres://admin:admin@localhost:5432/bank0_test?sslmode=disable'
go test -count=1 ./internal/db/ ./internal/api/
go test -tags e2e -count=1 ./internal/e2e/
```

If Docker Hub rate-limits you, the GCR mirror works:

```bash
docker run -d --name pg18 -e POSTGRES_USER=admin -e POSTGRES_PASSWORD=admin \
  -e POSTGRES_DB=bank0_test -p 5544:5432 mirror.gcr.io/library/postgres:18-alpine
```

CI runs `postgres:18` in every job that touches a database. The browser end-to-end
suite boots its own through the Playwright global setup.

---

## 8. Before you open a pull request

```bash
go build ./... && go vet ./...
task test                 # and task test:db, or the suite above against PG18
task webapp:build         # only if you touched web/app/
```

Commit generated code alongside its source. If you added a migration, confirm it
survives up, down and up on a throwaway database - `TestMigrationsReversible`
does this for you, but only when a DSN is set.
