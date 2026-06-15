import { test, expect } from "@playwright/test";
import { uiLogin } from "./fixtures";

// Changing a password is destructive (it persists + signs other devices out), so this
// test uses a dedicated seeded customer no other spec touches.
test("customer changes their password", async ({ page, request }) => {
  const user = "carol.carlsson"; // a dedicated seeded persona (db/seed.sql); first.last per the seed convention
  const newPass = "n3w-passw0rd-x"; // >= 12 chars (ChangePasswordRequest.new_password minLength)

  await uiLogin(page, user);
  await page.goto("/password");
  await page.fill("#cur", "password");
  await page.fill("#new", newPass);
  await page.fill("#conf", newPass);
  await page.getByRole("button", { name: "Change password" }).click();
  await expect(page.getByRole("heading", { name: "Password changed" })).toBeVisible();

  // The new password actually authenticates and the old one is rejected.
  const ok = await request.post("/api/auth/login", { data: { username: user, password: newPass } });
  expect(ok.ok(), "login with new password").toBeTruthy();
  const bad = await request.post("/api/auth/login", { data: { username: user, password: "password" } });
  expect(bad.ok(), "old password rejected").toBeFalsy();
});
