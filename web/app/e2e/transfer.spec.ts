import { test, expect } from "@playwright/test";
import { apiLogin, uiLogin } from "./fixtures";

test("customer adds a payee and completes a transfer end to end", async ({ page, request }) => {
  // You can't add your OWN account as a payee, so use another seeded customer's IBAN.
  const payeeIban = (await apiLogin(request, "bob")).accounts[0].iban;

  await uiLogin(page); // alice
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
