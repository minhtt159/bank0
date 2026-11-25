// Types `env` from "cloudflare:test" as the worker's own Env.
import type { Env as WorkerEnv } from "../index";

declare global {
  namespace Cloudflare {
    interface Env extends WorkerEnv {}
  }
}
