# Cloud Run + Supabase + Cloudflare — one-time provisioning

The repeatable deploy lives in [`.github/workflows/deploy.yml`](../../.github/workflows/deploy.yml).
This file is the **one-time setup** it depends on. Design rationale is in
[`docs/08-deployment-cloud-run-supabase.md`](../../docs/08-deployment-cloud-run-supabase.md).

Run these once (or keep as IaC later). Replace the placeholders:

```bash
export PROJECT_ID=your-gcp-project
export REGION=europe-west1            # near Supabase eu-central-1
export GAR_REPO=bank0
export GH_REPO=minhtt159/bank0        # owner/repo for Workload Identity
```

## 1. Enable APIs

```bash
gcloud config set project "$PROJECT_ID"
gcloud services enable \
  run.googleapis.com artifactregistry.googleapis.com cloudscheduler.googleapis.com \
  secretmanager.googleapis.com iamcredentials.googleapis.com
```

## 2. Artifact Registry (image target)

```bash
gcloud artifacts repositories create "$GAR_REPO" \
  --repository-format=docker --location="$REGION" \
  --description="bank0 images"
```

## 3. Secrets (read by the running services, not stored in GitHub)

`bank0-db-dsn` must be the **Supabase Supavisor session-pooler** DSN (port 5432).
`bank0-jwt-secret` is the shared HS256 key for the client API.

```bash
printf '%s' 'postgres://postgres.<ref>:<pw>@aws-0-eu-central-1.pooler.supabase.com:5432/postgres?sslmode=require' \
  | gcloud secrets create bank0-db-dsn --data-file=-
printf '%s' "$(openssl rand -hex 32)" \
  | gcloud secrets create bank0-jwt-secret --data-file=-
```

## 4. Deploy service account + Workload Identity Federation (keyless GitHub auth)

```bash
# Runtime/deploy identity
gcloud iam service-accounts create bank0-deployer --display-name="bank0 CI deployer"
SA="bank0-deployer@${PROJECT_ID}.iam.gserviceaccount.com"

for role in roles/run.admin roles/artifactregistry.writer \
            roles/iam.serviceAccountUser roles/secretmanager.secretAccessor \
            roles/cloudscheduler.admin; do
  gcloud projects add-iam-policy-binding "$PROJECT_ID" --member="serviceAccount:$SA" --role="$role"
done

# WIF pool + GitHub OIDC provider
gcloud iam workload-identity-pools create github --location=global --display-name="GitHub"
gcloud iam workload-identity-pools providers create-oidc github-oidc \
  --location=global --workload-identity-pool=github \
  --display-name="GitHub OIDC" \
  --attribute-mapping="google.subject=assertion.sub,attribute.repository=assertion.repository" \
  --attribute-condition="assertion.repository=='${GH_REPO}'" \
  --issuer-uri="https://token.actions.githubusercontent.com"

POOL_ID=$(gcloud iam workload-identity-pools describe github --location=global --format='value(name)')
gcloud iam service-accounts add-iam-policy-binding "$SA" \
  --role=roles/iam.workloadIdentityUser \
  --member="principalSet://iam.googleapis.com/${POOL_ID}/attribute.repository/${GH_REPO}"

echo "GCP_WIF_PROVIDER = ${POOL_ID}/providers/github-oidc"
echo "GCP_SERVICE_ACCOUNT = ${SA}"
```

## 5. GitHub repo configuration

**Settings → Environments → `prod`**: add **required reviewers** (this is the
approval gate the deploy jobs wait on).

**Variables** (Settings → Secrets and variables → Actions → Variables):

| Name | Value |
|---|---|
| `GCP_PROJECT_ID` | `$PROJECT_ID` |
| `GCP_REGION` | `$REGION` |
| `GAR_REPO` | `$GAR_REPO` |
| `GCP_WIF_PROVIDER` | printed in step 4 |
| `GCP_SERVICE_ACCOUNT` | printed in step 4 |

**Secrets**:

| Name | Value |
|---|---|
| `SUPABASE_SESSION_DSN` | same session-pooler DSN as `bank0-db-dsn` (used by the migrate job) |
| `CLOUDFLARE_API_TOKEN` | token with Workers Scripts: Edit |
| `CLOUDFLARE_ACCOUNT_ID` | your Cloudflare account id |

## 6. First deploy

Push to `main` (or run the **Deploy** workflow manually). Approve the `prod`
environment when prompted. The services are created on first deploy; note their
`*.run.app` URLs from the run output.

## 7. Maintenance Job schedule (Cloud Scheduler)

The Job is created/updated by CI; create its schedule once (every 2 min — the sweep
is advisory-locked, so cadence is safe to tune):

```bash
gcloud scheduler jobs create http bank0-maintenance-tick \
  --location="$REGION" --schedule="*/2 * * * *" \
  --uri="https://${REGION}-run.googleapis.com/apis/run.googleapis.com/v1/namespaces/${PROJECT_ID}/jobs/bank0-maintenance:run" \
  --http-method=POST \
  --oauth-service-account-email="bank0-deployer@${PROJECT_ID}.iam.gserviceaccount.com"
```

## 8. Domains & Cloudflare (docs/08 §3.2)

- **PWA** (`bank0.hnimn.art`): already a Worker; set its `API_ORIGIN`
  ([`worker/wrangler.toml`](../../worker/wrangler.toml)) to the api service URL
  (the `*.run.app` URL is fine — no custom domain needed for the proxy path).
- **`api.*` / `portal.*`**: front Cloud Run with Cloudflare. Quick path —
  `gcloud run domain-mappings create --service bank0-portal --domain portal.bank0.hnimn.art`
  then a Cloudflare proxied CNAME, SSL **Full (strict)**. Production path —
  Serverless NEG + external HTTPS Load Balancer, Cloudflare CNAME → LB IP.
  **Not** Cloudflare Tunnel (breaks scale-to-zero — see docs/08 §3.2).
- **Lock the origin to Cloudflare**: add a Transform Rule injecting a secret
  header at the edge and reject requests missing it, and/or allowlist Cloudflare
  IP ranges. Keeps `--allow-unauthenticated` services reachable only via the edge.
```
