import { test, expect, type Page } from "@playwright/test";

// alice/password is created by db/seed.sql (loaded in global-setup).
async function login(page: Page, user = "alice", pass = "password") {
  await page.goto("/login");
  await page.fill("#u", user);
  await page.fill("#p", pass);
  await page.getByRole("button", { name: /sign in/i }).click();
}

test("customer logs in and sees their accounts", async ({ page }) => {
  await login(page);
  await expect(page.getByRole("heading", { name: "Your accounts" })).toBeVisible();
  await expect(page.locator("a.card").first()).toBeVisible();
  await expect(page.locator(".iban").first()).toContainText(/NL\d{2}/);
});

test("wrong password shows an error and stays on /login", async ({ page }) => {
  await login(page, "alice", "definitely-wrong");
  await expect(page.locator(".error")).toBeVisible();
  await expect(page).toHaveURL(/\/login/);
});

test("unauthenticated visit bounces to /login", async ({ page }) => {
  await page.goto("/");
  await expect(page).toHaveURL(/\/login/);
  await expect(page.locator("#u")).toBeVisible();
});
