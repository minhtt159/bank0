import { execSync, spawn } from "node:child_process";
import { writeFileSync } from "node:fs";
import path from "node:path";
import { PG_CONTAINER, PG_PORT, API_PORT, DSN } from "./backend.config";

const REPO = path.resolve(process.cwd(), "../..");
const STATE = path.join(process.cwd(), "e2e", ".e2e-state.json");
const goEnv = { ...process.env, PATH: `${process.env.HOME}/go/bin:${process.env.PATH}` };
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

async function waitHttp(url: string, ms: number) {
  const end = Date.now() + ms;
  while (Date.now() < end) {
    try {
      const r = await fetch(url);
      if (r.ok) return;
    } catch {
      /* not up yet */
    }
    await sleep(500);
  }
  throw new Error(`timeout waiting for ${url}`);
}

export default async function globalSetup() {
  console.log("[e2e] (re)creating Postgres…");
  try { execSync(`docker rm -f ${PG_CONTAINER}`, { stdio: "ignore" }); } catch {}
  execSync(
    `docker run -d --name ${PG_CONTAINER} -e POSTGRES_USER=admin -e POSTGRES_PASSWORD=admin -e POSTGRES_DB=bank0 -p ${PG_PORT}:5432 postgres:18-alpine`,
    { stdio: "ignore" },
  );
  for (let i = 0; i < 60; i++) {
    try { execSync(`docker exec ${PG_CONTAINER} pg_isready -U admin -d bank0`, { stdio: "ignore" }); break; }
    catch { await sleep(1000); }
  }

  console.log("[e2e] building api binary…");
  execSync(`go build -o bin/bank0 ./cmd/app`, { cwd: REPO, stdio: "inherit", env: goEnv });

  console.log("[e2e] migrate + seed…");
  execSync(`./bin/bank0 migrate up`, { cwd: REPO, stdio: "inherit", env: { ...goEnv, APP_DATABASE_DSN: DSN } });
  execSync(`docker exec -i ${PG_CONTAINER} psql -U admin -d bank0 -v ON_ERROR_STOP=1 < db/seed.sql`, { cwd: REPO, stdio: "ignore" });

  console.log(`[e2e] starting api on :${API_PORT}…`);
  const api = spawn("./bin/bank0", [], {
    cwd: REPO, detached: true, stdio: "ignore",
    env: {
      ...process.env,
      APP_DATABASE_DSN: DSN,
      APP_SERVER_MODE: "api",
      APP_SERVER_PORT: String(API_PORT),
      APP_AUTH_JWT_SECRET: "e2e-secret",
      APP_ADMIN_RUN_MAINTENANCE: "false",
      APP_LOGGING_ENCODING: "json",
    },
  });
  api.unref();
  await waitHttp(`http://localhost:${API_PORT}/health`, 20_000);

  writeFileSync(STATE, JSON.stringify({ pg: PG_CONTAINER, apiPid: api.pid }));
  console.log("[e2e] backend ready");
}
