import { execSync } from "node:child_process";
import { existsSync, readFileSync, rmSync } from "node:fs";
import path from "node:path";

const STATE = path.join(process.cwd(), "e2e", ".e2e-state.json");

export default async function globalTeardown() {
  if (!existsSync(STATE)) return;
  const s = JSON.parse(readFileSync(STATE, "utf8"));
  try { if (s.apiPid) process.kill(s.apiPid, "SIGTERM"); } catch {}
  try { execSync(`docker rm -f ${s.pg}`, { stdio: "ignore" }); } catch {}
  rmSync(STATE, { force: true });
}
