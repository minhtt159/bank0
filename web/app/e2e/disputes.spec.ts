import { test, expect } from "@playwright/test";
import { apiLogin, postTransfer, uiLogin } from "./fixtures";

// Disputes are the headline new PWA feature. Set up a real POSTED transfer over the
// API (alice -> bob), then drive the UI: open its receipt, report a problem, and see
// the dispute land in "My disputes".
test("customer reports a problem on a payment and sees it in disputes", async ({ page, request }) => {
  const alice = await apiLogin(request, "alice");
  const bob = await apiLogin(request, "bob");
  const { transfer_id, status } = await postTransfer(request, alice, bob.accounts[0].id);
  expect(status).toBe("posted"); // under the maker-checker threshold -> settles immediately

  await uiLogin(page); // alice
  await page.goto(`/transfer/${transfer_id}`);

  // Raise a dispute from the receipt.
  await page.getByRole("button", { name: /report a problem/i }).click();
  await page.locator("#cat").selectOption("fraud");
  await page.fill("#rsn", "I did not authorise this");
  await page.getByRole("button", { name: /submit report/i }).click();
  await expect(page.getByText(/problem reported/i)).toBeVisible();

  // It appears in the disputes list.
  await page.goto("/disputes");
  await expect(page.getByRole("heading", { name: "My disputes" })).toBeVisible();
  await expect(page.getByText("Fraud")).toBeVisible();
});
