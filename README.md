# bank0

[![CI](https://github.com/minhtt159/bank0/actions/workflows/ci.yml/badge.svg)](https://github.com/minhtt159/bank0/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/minhtt159/bank0?sort=semver)](https://github.com/minhtt159/bank0/releases/latest)
[![Image](https://img.shields.io/badge/ghcr.io-bank0-blue?logo=docker&logoColor=white)](https://github.com/minhtt159/bank0/pkgs/container/bank0)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)

A **core-banking backend**: a double-entry ledger where correctness is a property of
the database, fronted by a thin Go API, an operator console, and a customer PWA. It
holds account balances and moves money between them without ever losing a cent,
double-spending, or double-posting on a retry.

Four invariants shape everything (see [`docs/01-overview.md`](docs/01-overview.md)):

1. **The ledger is the source of truth.** `accounts.balance_minor` is a
   trigger-maintained cache of `SUM(ledger_entries)`, always reconcilable; money is
   never created from nowhere.
2. **Money/auth logic lives in the database.** PL/pgSQL functions + triggers own every
   money movement and auth transition; the API is thin transport.
3. **Idempotency is enforced by the database.** Replays return the original result;
   they never double-post.
4. **Append-only and auditable.** The ledger can't be updated or deleted — corrections
   are new reversing entries.

## Three surfaces, three hosts

Two surfaces are the same Go binary in different `server.mode`s (separated in the app,
not just at the edge); the third is a Cloudflare Worker.

| Host | Surface | Tech | Auth |
|------|---------|------|------|
| `portal.bank0.hnimn.art` | admin API + operator console | Go `mode=portal` (Templ/HTMX) | DB cookie session (staff roles, 30-min idle) |
| `api.bank0.hnimn.art` | customer JSON API | Go `mode=api`, behind Cloudflare | JWT bearer + rotating refresh tokens (ownership-scoped) |
| `bank0.hnimn.art` | customer PWA | Cloudflare Worker (Preact/Vite) | proxies `/api/*` to the client API |

`server.mode=all` serves both Go surfaces in one container for local development.

## Quick start (local)

```bash
docker compose -f deploy/docker-compose.dev.yml up --build -d   # Postgres + migrate + admin (:8080) + client (:8090)
task seed                                                       # load the dev seed (db/seed.sql); migrate ran above
open http://localhost:8080/        # operator console (Templ + HTMX)
open http://localhost:8090/docs    # client API reference (Scalar)
```

Seeded logins (dev passwords): staff `admin`/`admin`, `operator1`/`operator`,
`auditor1`/`auditor`; customers `alice`/`password` … (no console access). The default
seed (`db/seed.sql`, idempotent) loads 98 customers / 242 accounts (valid NL IBANs) /
741 transfers, with pending/canceled/reversed lifecycle coverage and a randomized
10-user / 30-account guided-transfer "mule" pool; `task seed:demo` loads a larger
randomized set, and `task dev:reset` rebuilds the stack from a clean DB and seeds it in
one step. **Change the admin password before exposing the portal.**

Without Docker: `task install && task generate && task migrate:up && psql "$APP_DATABASE_DSN" -f db/seed.sql && task run`.

## Deploy

Self-hosted Kubernetes is the primary path — one image, one Helm chart, both
published to GHCR. Nothing needs to be built locally:

```bash
helm install bank0 oci://ghcr.io/minhtt159/charts/bank0 --version 1.0.0 \
  --set database.existingSecret=bank0-db \
  --set auth.existingSecret=bank0-auth
```

creates `bank0-api` (mode=api, HPA) and `bank0-portal` (mode=portal) behind Gateway
API/Envoy, with a pre-upgrade migrate job ([`docs/04-deployment.md`](docs/04-deployment.md)).

| Artifact | Where |
|---|---|
| Image (multi-arch: `linux/amd64` + `linux/arm64`) | `ghcr.io/minhtt159/bank0:1.0.0` — `sha-<commit>` on every `main` push, `X.Y.Z` + `X.Y` on `v*` tags. Never `latest`. |
| Chart | `oci://ghcr.io/minhtt159/charts/bank0` — published on `v*` tags |

CI publishes; it never deploys — `helm upgrade` stays an operator command
([`docs/04-deployment.md`](docs/04-deployment.md) §6). Per-surface Gateway attachment
and in-cluster PWA hosting are still open in
[`docs/specs/spec-container-helm-pivot.md`](docs/specs/spec-container-helm-pivot.md).

## Tech stack

Go 1.26 · PostgreSQL 18 (native `uuidv7()`; 18 is the floor) ·
pgx/v5 + sqlc · goose migrations · slog · BIGINT minor units · bcrypt (pgcrypto) ·
Templ + HTMX (console) · OpenAPI 3.1 contract-first (oapi-codegen + Scalar) · Helm.

## Documentation

Start at [`docs/01-overview.md`](docs/01-overview.md) — it frames the design and walks
through how you use bank0 (the customer money-move and the operator journeys). The
reference docs ([`docs/02`](docs/02-data-model.md)–[`docs/12`](docs/12-rail-readiness.md), no `08`)
cover the data model, ledger lifecycle, deployment, the two surfaces, the PWA, fraudbank
integration, security, IBAN handling, and the closed-core-to-rail readiness seam. The
product roadmap is in [`docs/specs/`](docs/specs/).

## Releases

Versions are semver git tags; each one publishes the image and the chart, and the
GitHub Release carries the notes. There is no `CHANGELOG.md` by convention — the
release notes and the PRs behind them are the record, which is also what
dependency bots (Renovate et al.) surface when they propose a bump.

## License

[Apache License 2.0](LICENSE). Copyright 2026 Minh Tran.

bank0 is a portfolio/demonstration core-banking backend. It is not a licensed
financial institution, holds no real money, and connects to no payment rail — the
IBANs it mints are internally valid but not routable ([`docs/12`](docs/12-rail-readiness.md)
covers what real-rail readiness would take).
