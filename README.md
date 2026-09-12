# bank0

[![CI](https://github.com/minhtt159/bank0/actions/workflows/ci.yml/badge.svg)](https://github.com/minhtt159/bank0/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/minhtt159/bank0?sort=semver)](https://github.com/minhtt159/bank0/releases/latest)
[![Image](https://img.shields.io/badge/ghcr.io-bank0-blue?logo=docker&logoColor=white)](https://github.com/minhtt159/bank0/pkgs/container/bank0)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)

A core-banking backend: a double-entry ledger where correctness is a property of
the database, fronted by a thin Go API, an operator console, and a customer PWA.
It holds account balances and moves money between them without losing a cent,
spending the same money twice, or posting a payment twice because the client
retried.

Double-entry means every movement is recorded twice - a debit on one account and
an equal credit on another - so the books balance by construction rather than by
a nightly job. Amounts are integers in minor units (EUR 12.34 is `1234`), never
floats.

The unusual part is where the logic lives. Every money movement and every auth
transition is a PL/pgSQL function holding explicit row locks; the Go handlers
parse a request, call exactly one of those functions, and map its result to an
HTTP status. A second client, a cron job or a `psql` session all get the same
guarantees, because the guarantees are in the schema.
[`docs/01-overview.md`](docs/01-overview.md) explains why.

## Run it

```bash
docker compose -f deploy/docker-compose.dev.yml up --build -d
task seed
open http://localhost:8080/        # operator console
open http://localhost:8090/docs    # client API reference
```

Compose brings up Postgres 18, runs the migrations, and starts both Go surfaces:
the operator console on `:8080` and the customer API on `:8090`. `task seed`
loads the dev data - 98 customers, 242 accounts with valid NL IBANs, 741
transfers covering the pending, canceled and reversed paths.

Sign in to the console as `admin` / `admin`. It will make you change that
password before it lets you do anything else, because the seeded one is
published in this repository. Customers sign in to the PWA with
`alice` / `password` and have no console access. `task seed:demo` loads a much
larger randomized set; `task dev:reset` rebuilds from a clean database in one
step.

Prefer no Docker:
`task install && task generate && task migrate:up && psql "$APP_DATABASE_DSN" -f db/seed.sql && task run`.
Working on the code rather than running it:
[`docs/08-development.md`](docs/08-development.md).

## The three surfaces

| Host | Surface | Auth |
|---|---|---|
| `portal.bank0.hnimn.art` | admin API + operator console | cookie session, staff roles |
| `api.bank0.hnimn.art` | customer JSON API | JWT bearer + rotating refresh tokens |
| `bank0.hnimn.art` | customer PWA | the Worker proxies `/api/*` to the client API |

The first two are the same Go binary in different `server.mode`s - separated in
the application, not just at the edge, so an `api` pod never registers an admin
route. The third is a Cloudflare Worker. A third mode, `all`, serves both Go
surfaces from one process; `task run` uses it, while the compose stack above runs
the two modes as separate containers, the way production does.

## Deploy it

Self-hosted Kubernetes is the primary path. One image and one chart, both
published to GHCR, so nothing is built locally:

```bash
helm install bank0 oci://ghcr.io/minhtt159/charts/bank0 --version 1.0.2 \
  --set database.existingSecret=bank0-db \
  --set auth.existingSecret=bank0-auth
```

That creates `bank0-api` (mode=api, with an HPA) and `bank0-portal` behind
Gateway API, with migrations as a pre-upgrade job. The image is multi-arch
(`linux/amd64` + `linux/arm64`) at `ghcr.io/minhtt159/bank0`, tagged
`sha-<commit>` on every `main` push and `X.Y.Z` + `X.Y` on version tags - never
`latest`. CI publishes but never deploys: `helm upgrade` stays an operator
command. Details in [`docs/04-deployment.md`](docs/04-deployment.md).

## Built with

Go 1.27, PostgreSQL 18 (the floor - the schema uses the native `uuidv7()`),
pgx/v5 with sqlc, goose migrations, BIGINT minor units, bcrypt via pgcrypto,
Templ and HTMX for the console, an OpenAPI 3.1 contract with oapi-codegen and
Scalar, Preact for the PWA, Helm for delivery.

## Documentation

Start at [`docs/01-overview.md`](docs/01-overview.md): it explains the four
invariants everything else follows from, and carries the map of which document
answers which question. Contributors want
[`docs/08-development.md`](docs/08-development.md). Integrating a client against
the API: [`docs/09-fraudbank-integration.md`](docs/09-fraudbank-integration.md).

## Releases

Versions are semver git tags; each publishes the image and the chart, and the
GitHub Release carries the notes. There is deliberately no `CHANGELOG.md` - the
release notes and the pull requests behind them are the record, and they are
what dependency bots surface when they propose a bump.

## License

[Apache License 2.0](LICENSE). Copyright 2026 Minh Tran.

bank0 is a portfolio and demonstration core-banking backend. It is not a
licensed financial institution, holds no real money, and connects to no payment
rail - the IBANs it mints are internally valid but not routable.
[`docs/12-rail-readiness.md`](docs/12-rail-readiness.md) covers what connecting
one would take.
