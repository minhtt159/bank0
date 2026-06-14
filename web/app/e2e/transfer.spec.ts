import { test, expect, type Page, type APIRequestContext } from "@playwright/test";

async function login(page: Page, user = "alice", pass = "password") {
  await page.goto("/login");
  await page.fill("#u", user);
  await page.fill("#p", pass);
  await page.getByRole("button", { name: /sign in/i }).click();
  await expect(page.getByRole("heading", { name: "Your accounts" })).toBeVisible();
}

// You can't add your OWN account as a payee, so grab another seeded customer's IBAN
// via the API (login as them, read their accounts) — API setup, UI assertion.
async function othersIban(request: APIRequestContext, who = "bob"): Promise<string> {
  const r = await request.post("/api/auth/login", { data: { username: who, password: "password" } });
  expect(r.ok()).toBeTruthy();
  const { token, user_id } = await r.json();
  const a = await request.get(`/api/users/${user_id}/accounts`, { headers: { Authorization: `Bearer ${token}` } });
  const accts = await a.json();
  expect(accts.length).toBeGreaterThan(0);
  return accts[0].iban as string;
}

test("customer adds a payee and completes a transfer end to end", async ({ page, request }) => {
  const payeeIban = await othersIban(request, "bob");

  await login(page); // alice
  await page.goto("/transfer");
  await expect(page.getByRole("heading", { name: "Send money" })).toBeVisible();
  await expect(page.locator(".pick.sel")).toBeVisible(); // default source pre-selected

  // Add bob as a payee (resolve / confirmation-of-payee), then send.
  await page.getByRole("button", { name: "+ Add payee" }).click();
  await page.locator(".card input").first().fill("Bob");
  await page.locator(".card input.iban").fill(payeeIban);
  await page.getByRole("button", { name: /look up/i }).click();
  await expect(page.getByText(/confirmation of payee/i)).toBeVisible();
  await page.getByRole("button", { name: /save payee/i }).click();

  await page.getByPlaceholder("0.00").fill("1.00");
  await page.getByRole("button", { name: /^review$/i }).click();
  await expect(page.getByRole("heading", { name: "Confirm transfer" })).toBeVisible();
  await page.getByRole("button", { name: /^send/i }).click();

  await expect(page).toHaveURL(/\/transfer\/[0-9a-f-]{36}/);
});
