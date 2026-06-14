import { expect, type APIRequestContext, type Page } from "@playwright/test";
import { randomUUID } from "node:crypto";

// Shared helpers for the PWA e2e specs. The pattern throughout: drive money/identity
// SETUP over the real client API (fast, deterministic), then ASSERT through the UI.

export const PASSWORD = "password"; // seeded customer password (db/seed.sql)

// Log a seeded customer in through the PWA and land on the accounts home.
export async function uiLogin(page: Page, user = "alice", pass = PASSWORD) {
  await page.goto("/login");
  await page.fill("#u", user);
  await page.fill("#p", pass);
  await page.getByRole("button", { name: /sign in/i }).click();
  await expect(page.getByRole("heading", { name: "Your accounts" })).toBeVisible();
}

export type ApiSession = {
  token: string;
  userId: string;
  accounts: { id: string; iban: string }[];
};

// Authenticate a seeded customer over the API and return a bearer token + their
// accounts. Each call mints a fresh refresh-token family (a "device").
export async function apiLogin(
  request: APIRequestContext,
  user: string,
  pass = PASSWORD,
): Promise<ApiSession> {
  const r = await request.post("/api/auth/login", { data: { username: user, password: pass } });
  expect(r.ok(), `login ${user}`).toBeTruthy();
  const { token, user_id } = await r.json();
  const a = await request.get(`/api/users/${user_id}/accounts`, {
    headers: { Authorization: `Bearer ${token}` },
  });
  expect(a.ok(), `accounts ${user}`).toBeTruthy();
  const accounts = await a.json();
  expect(accounts.length, `${user} has an account`).toBeGreaterThan(0);
  return { token, userId: user_id as string, accounts };
}

// Post a transfer over the API. A small amount stays under the maker-checker
// threshold (€10,000), so it posts immediately rather than queuing for approval.
export async function postTransfer(
  request: APIRequestContext,
  payer: ApiSession,
  creditAccountId: string,
  amountMinor = 100,
): Promise<{ transfer_id: string; status: string; was_replay: boolean }> {
  const r = await request.post("/api/transfers", {
    headers: { Authorization: `Bearer ${payer.token}`, "Idempotency-Key": randomUUID() },
    data: {
      debit_account: payer.accounts[0].id,
      credit_account: creditAccountId,
      amount_minor: amountMinor,
      description: "e2e",
    },
  });
  expect(r.ok(), "post transfer").toBeTruthy();
  return r.json();
}
