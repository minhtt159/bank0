import { test, expect } from "@playwright/test";
import { uiLogin } from "./fixtures";

// Activity: alice carries a long seeded statement, so the list, the sent/received
// filter, and opening a row into its receipt are all exercisable.
test("customer browses activity, filters, and opens a payment", async ({ page }) => {
  await uiLogin(page); // alice
  await page.goto("/activity");
  await expect(page.getByRole("heading", { name: "Activity" })).toBeVisible();
  await expect(page.locator("a.list-item").first()).toBeVisible();

  await page.getByRole("button", { name: "Sent" }).click();
  await expect(page.locator("a.list-item").first()).toBeVisible();

  await page.locator("a.list-item").first().click();
  await expect(page).toHaveURL(/\/transfer\/[0-9a-f-]{36}/);
});

// Devices: the current session is marked, and another (API-created) session can be
// revoked away from the list.
test("customer sees their devices and revokes another session", async ({ page, request }) => {
  await uiLogin(page); // alice — this browser is the current device
  // A second sign-in over the API mints another (revocable, non-current) device.
  await request.post("/api/auth/login", { data: { username: "alice", password: "password" } });

  await page.goto("/devices");
  await expect(page.getByRole("heading", { name: /devices/i })).toBeVisible();
  await expect(page.getByText("this device")).toBeVisible();

  const before = await page.locator(".card").count();
  expect(before).toBeGreaterThan(1);
  await page.getByRole("button", { name: "Revoke" }).first().click();
  await expect(page.locator(".card")).toHaveCount(before - 1);
});
