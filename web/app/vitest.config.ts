import { defineConfig } from "vitest/config";

// Unit tests for pure PWA logic (e.g. IBAN validation). Scoped to src/ so vitest
// never picks up the Playwright specs under e2e/ — those run via `playwright test`.
export default defineConfig({
  test: {
    include: ["src/**/*.test.ts"],
    environment: "node",
  },
});
