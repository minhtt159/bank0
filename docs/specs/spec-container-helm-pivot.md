# Spec — Container images + Helm chart pivot (self-hosted Kubernetes)

> **Status: planning.** The deployment story pivoted: the managed/serverless path
> (Supabase + Cloud Run + Cloudflare) is **gone** — `docs/08`, `deploy/cloudrun/`
> and `.github/workflows/deploy.yml` were deleted, and PG17 support went with it.
> The only target is **published container images + the existing Helm chart** on a
> self-hosted cluster with its own Postgres 18. This spec is the gap
> analysis and the ordered work plan — nothing here is built yet, and per the
> `docs/specs/` convention it is retired into the reference docs
> ([`../04-deployment.md`](../04-deployment.md), [`../07-client-web-app.md`](../07-client-web-app.md))
> once shipped.
>
> The good news up front: **most of the work already exists.**
> [`deploy/Dockerfile`](../../deploy/Dockerfile) is a clean distroless/nonroot
> image and [`deploy/helm/bank0/`](../../deploy/helm/bank0/) is a real chart
> (split deployments, hook-Job migrations, correct probes, hardening,
> ServiceMonitor/dashboard). The gaps are at the edges: one admission-policy
> blocker in the migrate Job, a single-Gateway assumption that doesn't fit the
> target cluster, no image/chart publishing, and a homeless PWA.

---

## 0. What changes and what doesn't

| | |
|---|---|
| **Unchanged** | The application. Same binary, same run modes (`api`/`portal`), same embedded goose migrations, same `/health`·`/readyz`·`/metrics` contract. Zero Go/SQL changes are required by this pivot (the PWA option B in §5 would be the only exception, and it is not the recommended option). |
| **Unchanged** | The chart's core shape: two Deployments from one image, pre-upgrade migrate hook Job, HTTPRoutes via Gateway API, advisory-locked in-process maintenance on portal pods. The serverless-only workaround (Cloud Scheduler + `bank0 maintenance` Job) is simply not needed here. |
| **Changes** | `deploy.yml` was deleted outright (it only ever deployed the dead path); W2/W4 recreate it as a publisher (W2: image → GHCR, W4: chart → GHCR OCI). |
| **Changes** | The chart grows the small knobs a real shared cluster needs: admission-clean hook Job, `imagePullSecrets`, per-surface Gateway attachment. |
| **Changes** | The PWA needs an in-cluster home (§5) — the only genuinely new piece. |
| **Deleted** | Everything Supabase/Cloud Run: `docs/08`, `deploy/cloudrun/`, `.github/workflows/deploy.yml`, the PG17 `uuidv7()` polyfill in `00001_foundation.sql`, and every PG17 CI leg. PG18 is the floor. |
| **Kept for now** | `worker/` — the Cloudflare Worker still hosts the PWA and is its `/api` proxy-contract reference; its CI test still runs. It retires when §5 lands, not before. |

## 1. Target environment (assumed; confirm in §8)

The point of the pivot is **deploy-anywhere**: the image and chart must install on
any conformant cluster — a self-hosted homelab or a managed cloud one (EKS et al.)
— with only an install-time values file changing. The reference profile below is
deliberately the *strictest* plausible cluster (real admission control, shared
platform-owned Gateways), because a chart that clears it installs anywhere; no
environment-specific names or files belong in this repo:

- **Two Gateways (Gateway API), two GatewayClasses:** an **external** one
  (internet-facing, behind tunnel ingress — a tunnel is fine here, unlike on a
  scale-to-zero platform, because the Gateway is always-on) and an **internal**
  one (private-network only, resolved by the operator's internal DNS). Intended
  split: `api.*` on external, `portal.*` on internal.
- **Admission policies (enforce mode)** rejecting: `:latest` image tags, pods
  without resource requests, and default service-account token automount.
- **Postgres 18**, provisioned outside this chart (the chart stays BYO-DSN — it
  must not grow a Postgres subchart).
- Developer machines are **arm64**; cluster node arch **unconfirmed** (§4).

## 2. Gap analysis — the image ([`deploy/Dockerfile`](../../deploy/Dockerfile))

**Verdict: keep as-is.** Multi-stage Go 1.26 build, `CGO_ENABLED=0`, runtime is
`gcr.io/distroless/static-debian12:nonroot` (uid 65532), copies only the binary +
`config.yaml` + `api/openapi.yaml`, port 8080, `ENTRYPOINT bank0` with `serve` /
`migrate` / `maintenance` subcommands — one image already serves api, portal, and
the migrate Job. A `.dockerignore` exists, and generated code is committed, so the
build needs no toolchain beyond Go.

Two build-time notes, not image defects:

1. **Multi-arch:** the Dockerfile builds whatever platform invokes it. For a
   two-arch manifest without QEMU-slow compiles, the build stage should become
   `FROM --platform=$BUILDPLATFORM golang:1.26` + `GOOS=linux GOARCH=$TARGETARCH`
   on the `go build` line (Go cross-compiles for free with CGO off). ~2-line
   change, done in W2 (§7).
2. **Tag hygiene:** never publish `:latest` — the target cluster's admission control rejects it anyway (§4).

## 3. Gap analysis — the chart ([`deploy/helm/bank0/`](../../deploy/helm/bank0/))

What a self-hosted deploy needs vs. what `helm template` renders today. ✅ = fine
as-is, ❌ = gap (with priority/effort in the repo's `P0–P2` / `S/M/L` scale).

| Concern | Today | Verdict |
|---|---|---|
| **No `:latest`** (admission) | `bank0.image` helper renders `repo:{{ tag \| default .Chart.AppVersion }}` → `ghcr.io/minhtt159/bank0:1.0.0`. Never `latest`. | ✅ |
| **Resource requests** (admission) — Deployments | Both api and portal set `resources.requests` + `limits` from values. | ✅ |
| **Resource requests** (admission) — **migrate hook Job** | [`migrate-job.yaml`](../../deploy/helm/bank0/templates/migrate-job.yaml) sets **no `resources`**. the resource-requests policy **rejects the hook pod → every `helm install`/`upgrade` on the target cluster fails before anything else runs.** | ❌ **P0, S** |
| **SA token automount** (admission) — Deployments | Both pods set `automountServiceAccountToken: false`. | ✅ |
| **SA token automount** (admission) — **migrate hook Job** | Not set on the Job pod → the SA-token-automount policy rejects it (same blast radius as above). While in there: the Job pod also lacks the `podSecurityContext`/`securityContext` both Deployments apply — same image, no reason to run the hook softer. | ❌ **P0, S** |
| **Image pull secrets** | No `imagePullSecrets` plumbing anywhere (helper, deployments, Job). Blocks a **private** GHCR package entirely; harmless if the package is public (§8 Q2). | ❌ **P1, S** |
| **Probes** | Liveness → `/health` (DB-blind), readiness → `/readyz` (DB-aware, 1s deadline), both Deployments, with the rationale in comments. Exactly right. | ✅ |
| **Secrets** | `existingSecret` pattern for both DSN and JWT; chart-created Secret only as a dev convenience. Correct — how the operator materializes the secrets (kubectl / sealed-secrets / SOPS) is their business (§8 Q5); the chart must **not** grow ExternalSecrets/CSI support speculatively. | ✅ |
| **Migrations** | `pre-install,pre-upgrade` hook Job, weight −5, `before-hook-creation,hook-succeeded` delete policy, `backoffLimit: 3` — DB-before-app preserved by Helm ordering, replacing the CI `migrate` job of the deferred path. | ✅ (modulo the two admission fixes above) |
| **Gateway attachment** | One `gateway:` block: either the chart **creates** a Gateway (class `eg`, per-host TLS listeners, cert-manager gateway-shim annotation) or both HTTPRoutes attach to **one** shared Gateway — and `sectionName` is hardcoded to the chart's own listener names (`https-api`/`https-portal`/`http`). The target cluster has **two platform-owned Gateways** with their own listener names, and api/portal must attach to **different** ones. | ❌ **P0, M** — per-surface `parentRef` (`gateway.name/namespace/sectionName`) in values, defaulting to today's single-gateway behavior (see W3, §7) |
| **TLS / redirect** | Chart-owned-Gateway features: per-listener certs via cert-manager, `httpsRedirect` route on the `http` listener. On a shared-Gateway cluster TLS terminates at the edge (tunnel/LB) or at the platform Gateways; these features must simply be **off** in shared-gateway mode — they already are gated, just document it. | ✅ (docs only) |
| **Observability** | ServiceMonitor + Grafana dashboard ConfigMap ([`dashboards/bank0-overview.json`](../../deploy/helm/bank0/dashboards/bank0-overview.json)), both off by default because they need operator CRDs. Fits a kube-prometheus-stack cluster as-is — flip the two values. | ✅ |
| **`/metrics` exposure** | docs/04 says "restrict at the network layer" — implicitly Cloudflare's WAF. Attached to the external Gateway through the tunnel, `/metrics` on `api.*` is on the public internet. Cheap fix: an HTTPRoute rule matching `/metrics` (and nothing else) that never leaves the Gateway on external routes; or accept it (it leaks RED metrics, not data). | ❌ **P2, S** — decide (§8 Q7) |
| **Sizing defaults** | api HPA 3–10 + portal ×2 is prod-ish; fine as chart defaults. The operator overrides at install time (`-f` a local values file) — do **not** commit an environment values file unless the operator wants it in-repo. | ✅ |
| **HPA** | CPU-based `autoscaling/v2`, needs metrics-server — present in any real cluster; `autoscaling.enabled=false` falls back to `replicaCount`. | ✅ |
| **Chart/app versioning** | `Chart.yaml` is `version: 1.0.0` / `appVersion: "1.0.0"` (bumped for the release) but nothing publishes it. Fine while the chart is copy-installed from the repo; publishing (§6/W4) needs a bump-on-release discipline. | ❌ **P1, S** (publish half) |
| **PWA** | Not in the chart at all — it lived on the (deferred) Worker. | ❌ **P1, M** — §5 |

## 4. Image strategy

- **Registry: GHCR.** `values.yaml` already defaults to `ghcr.io/minhtt159/bank0`
  — the decision is half-made; finish it. GitHub Actions pushes with the ambient
  `GITHUB_TOKEN` (`packages: write`), no new secrets, no new infra. A self-hosted
  registry is rejected: it is one more stateful service to run, back up, and
  authenticate against, for zero benefit at this scale (YAGNI).
- **Tagging:** two kinds, produced by `docker/metadata-action`:
  - `sha-<shortsha>` on every push to `main` — the "deploy whatever main is"
    handle (`helm upgrade … --set image.tag=sha-…`).
  - `vX.Y.Z` (+ `X.Y`) on git tags — what a chart release's `appVersion` pins.
  - **No `latest`, ever** — the cluster's no-`:latest` admission policy enforces what is good
    practice anyway (an unpinned tag defeats "what exactly is running?").
- **Multi-arch: build `linux/amd64` + `linux/arm64`.** The operator develops on
  arm64 dev machines (so local `docker run` of the published image should just work) and
  the **cluster node arch is unconfirmed** (§8 Q1) — a two-arch manifest makes
  the question moot for ~1 extra minute of CI, using the `$BUILDPLATFORM`
  cross-compile from §2 (no QEMU emulation of the Go compiler).
- **PWA assets:** no longer wrangler-deployed. They become a second, tiny image
  (`ghcr.io/minhtt159/bank0-web` — option A below) built from `web/app/dist`,
  published by the same workflow, same tagging scheme. If option B is chosen
  instead, they ride inside the Go image and there is no second image.

## 5. Where the PWA goes (decided: option A, **internal Gateway**)

**Decision (2026-08-08):** the bank0 PWA moves in-cluster per option A below, but
attaches to the **internal Gateway** (private network), not the external one. The
public-facing web client is **fraudbank** (the sibling repo), which will deploy
later on its own Cloudflare Worker — bank0's own PWA is a dev/demo surface, not
the product front door. `worker/` stays untouched until fraudbank's Worker
deployment exists; do not migrate or retire it as part of this pivot.

The Worker did two jobs ([`../07-client-web-app.md`](../07-client-web-app.md) §2):
serve the built SPA, and proxy `/api/*` → the client API so the browser stays
same-origin (no CORS, no tokens crossing a third origin). Whatever replaces it
must preserve **exactly that contract** — the SPA speaks `/api/*` and must not
change (`web/app/` is untouched by this pivot; builds only).

| Option | Shape | Verdict |
|---|---|---|
| **A — static image + Gateway does the proxy** | A `deploy/Dockerfile.web` (nginx-unprivileged or equivalent static server, SPA fallback via `try_files`) serving `web/app/dist`; a third Deployment/Service in the chart; one HTTPRoute on the PWA host with **two rules**: `/api` prefix → `URLRewrite ReplacePrefixMatch` (strips `/api`) → `bank0-api` Service; catch-all → the web Service. Envoy Gateway supports this filter natively. | **Recommended.** Zero Go changes, zero SPA changes, the proxy moves from Worker JS to Gateway config. Cost: one more tiny image + ~3 small templates. |
| **B — Go binary embeds the SPA** | `embed.FS` of `dist/` served by the api binary (new mode or a path guard). | Rejected: couples every SPA tweak to a Go image rebuild, the mux isn't host-aware, and it's the only option that violates "the app is unchanged by this pivot". |
| **C — keep the Worker for the PWA only** | Everything else moves in-cluster; `bank0.hnimn.art` stays on Cloudflare. | Rejected as the target: it keeps the wrangler deploy leg and Cloudflare coupling alive, contradicting the pivot. It **is** the implicit interim state until W5 ships — the Worker keeps working meanwhile. |

Flag: the Worker was also the seam for a future token-holding BFF (httpOnly
refresh cookie, docs/07 §2). Option A keeps an equivalent seam (the Gateway or a
future thin proxy container), but the BFF idea is deferred along with the Worker —
noted so it isn't silently lost.

## 6. CI/CD — what replaces `deploy.yml`

[`ci.yml`](../../.github/workflows/ci.yml) keeps its shape: build/vet/test,
generate-drift, worker + webapp tests, e2e tiers, migration-reversibility. Its
PG17 legs are already gone (every job is `postgres:18`); the `worker` job retires
if/when `worker/` does.

`deploy.yml` is **deleted** — it was 100% the dead path. W2/W4 recreate it as a
publisher. What each of its jobs turned into:

| Was | Fate |
|---|---|
| `build-push` → GCP Artifact Registry (WIF auth, gcloud) | **Retired** → buildx multi-arch push to **GHCR** (`docker/login-action` + `metadata-action` + `build-push-action`, `GITHUB_TOKEN` only) |
| `migrate` (goose up → Supabase from CI) | **Retired** — migrations are the chart's pre-upgrade hook Job; DB-before-app is preserved by Helm, not CI |
| `deploy-api` / `deploy-portal` (gcloud run deploy) | **Retired** — no CI-driven deploy at all: the cluster sits behind a tunnel and CI has no path to it. The operator runs `helm upgrade`; GitOps is §8 Q6, not now |
| `deploy-maintenance` (Cloud Run Job + Scheduler) | **Retired** — maintenance is in-process on portal pods again (advisory-locked); the subcommand still exists if a CronJob is ever preferred |
| `deploy-pwa` (wrangler) | **Retired** → the `bank0-web` image build (option A), path-filtered on `web/app/**` as today |
| *(new)* chart publish | On git tag: `helm package deploy/helm/bank0` + `helm push oci://ghcr.io/minhtt159/charts` — GHCR speaks OCI, so no chart-releaser/gh-pages machinery |
| `prod` Environment gate | **Kept** on the publish jobs if desired (publishing is low-stakes; the real gate becomes the operator's own `helm upgrade`) — operator's call |
| Secrets: `SUPABASE_SESSION_DSN`, `GCP_*`, `CLOUDFLARE_*` | **All retired.** The workflow needs only `GITHUB_TOKEN` |

## 7. Work plan — small steps, each independently mergeable

| Wave | What | Checkpoint (must pass before the next wave) |
|---|---|---|
| **W1** | **Chart admission-compliance + pull secrets** (the two ❌ P0-S + P1-S rows of §3): migrate hook Job gets `resources`, `automountServiceAccountToken: false`, and both securityContexts; add `image.pullSecrets` plumbed into all three pod specs. | `helm template` diff reviewed; rendered manifests pass the three admission policies (policy-engine dry-run locally, or a throwaway install on the cluster). Default rendering otherwise byte-identical. |
| **W2** | **Image publish**: rewrite `deploy.yml` → GHCR multi-arch build/push (`sha-` on main, semver on tags), including the 2-line `$BUILDPLATFORM` Dockerfile tweak (§2). GCP/Cloudflare jobs and secrets deleted (the retired path is recorded in §0 — `docs/08` itself is gone). | `docker run ghcr.io/minhtt159/bank0:sha-… serve` answers `/health` on both arches; no `latest` tag exists on the package. |
| **W3** | **Per-surface Gateway attachment**: `api.gateway`/`portal.gateway` (or per-surface `parentRef`) values — name/namespace/sectionName each — defaulting to the current single-`gateway:` block so existing installs render unchanged; document the shared-gateway mode (TLS/redirect off, listener names from the platform Gateways). | Golden `helm template` unchanged with default values; with the operator's values the api route parents the external Gateway and the portal route parents the internal one. |
| **W4** | **Chart publish + version discipline**: tag-triggered `helm package`/`push` to GHCR OCI; `Chart.yaml` version/appVersion bump becomes part of tagging a release. | `helm install bank0 oci://ghcr.io/minhtt159/charts/bank0 --version …` works from a machine that has never seen the repo. |
| **W5** | **PWA in-cluster** (§5, decided: option A on the **internal Gateway**): `deploy/Dockerfile.web`, web Deployment/Service, PWA-host HTTPRoute (internal Gateway) with the `/api` `URLRewrite` rule; `deploy-pwa` job becomes the web-image build. `web/app/` source untouched; `worker/` untouched until fraudbank's own Worker deploy exists. | Login + a transfer completed through the PWA host (LAN) with the browser only ever talking same-origin `/api/*`; Worker still deployable as fallback until this soaks. |
| **W6** | **First real install** (operator-driven, no code): create the `bank0-db` + JWT secrets, point the tunnel hostnames at the external Gateway and internal DNS at the internal one, `helm install` with a local values file. | `/readyz` green on both surfaces, console login, seeded transfer, `reconcile()` clean, Grafana dashboard populated. Then: update docs/04 §§0/3 + docs/07 to as-built and **retire this spec** (repo convention: no archive). |
| **W7** *(optional)* | Cleanups per §8 answers: `/metrics` external exposure, `worker/` fate, GitOps. | — |

W1–W4 are strictly ordered but each ships alone; W5 can proceed in parallel after
W2 (it needs the registry, not the gateway work).

## 8. Open questions — decisions needed from the operator

| # | Question | Default if unanswered |
|---|---|---|
| Q1 | **Cluster node architecture?** (amd64 / arm64 / mixed) | Build both (§4) — cheap and makes it moot |
| Q2 | **GHCR packages public or private?** Private needs a pull secret in-cluster (W1 adds the plumbing either way). | Public (it's already a public repo) |
| Q3 | ~~PWA option A/B/C?~~ **Decided:** A, on the internal Gateway — the public web client is fraudbank (own Worker, later); bank0's PWA is internal/demo | — |
| Q4 | **Where does Postgres 18 run?** (CNPG in-cluster, a VM, …) The chart stays BYO-DSN regardless — this only affects the DSN the operator writes into the secret, and backup ownership. | Operator's existing Postgres; **no** Postgres subchart |
| Q5 | **How are secrets materialized** on the cluster (plain `kubectl create secret`, sealed-secrets, SOPS)? Chart supports all via `existingSecret`. | `kubectl` from the operator's vault |
| Q6 | **GitOps (Flux/Argo watching the OCI chart) or manual `helm upgrade`?** | Manual — smallest thing that works; revisit when upgrades become frequent enough to hurt |
| Q7 | **`/metrics` on the external Gateway** — block at the HTTPRoute, or accept exposure? | Block on external routes (one small rule), leave open internally |
| Q8 | **Listener names / `allowedRoutes` on the two platform Gateways** — needed verbatim for the W3 `sectionName`s, and their namespaces must permit routes from the bank0 namespace. | Blocks W3's install-time values (not the chart change itself) |
| Q9 | **Hosts stay `*.bank0.hnimn.art`?** (tunnel + internal DNS records are operator-side either way) | Yes |
