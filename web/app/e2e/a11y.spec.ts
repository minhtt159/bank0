import { test, expect } from "@playwright/test";
import { uiLogin } from "./fixtures";

// Guards the A11Y-PICKER-DIV fix: the transfer source/payee selectors used to be
// click-only <div onClick> (no keyboard focus, no screen-reader semantics). They
// are now native radios inside a labelled radiogroup.
test("transfer source picker is a keyboard/screen-reader accessible radiogroup", async ({ page }) => {
  await uiLogin(page);
  await page.goto("/transfer");
  await expect(page.getByRole("heading", { name: "Send money" })).toBeVisible();

  const group = page.getByRole("radiogroup", { name: "Source account" });
  await expect(group).toBeVisible();

  const radios = group.getByRole("radio");
  await expect(radios).not.toHaveCount(0); // accounts exposed as radio options
  // Exactly one is pre-selected (the default account — not necessarily the first).
  await expect(group.getByRole("radio", { checked: true })).toHaveCount(1);
  await radios.first().focus();
  await expect(radios.first()).toBeFocused(); // keyboard-focusable (was impossible before)
});
