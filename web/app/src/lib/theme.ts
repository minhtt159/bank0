// Catppuccin flavour selection, shared with the operator console: the same five
// choices and the same localStorage key ("bank0-theme"), so a flavour picked on
// one bank0 surface is the one the other offers too.
//
// "system" stores nothing and removes the attribute, which is what makes the
// prefers-color-scheme block in styles.css apply (`:root:not([data-theme])`).
//
// The first paint is handled by the inline script in index.html — it must run
// before the stylesheet or the page flashes Latte on a dark phone. This module
// only owns changes made after boot.

export const THEME_KEY = "bank0-theme";

export const FLAVOURS = ["system", "latte", "frappe", "macchiato", "mocha"] as const;
export type Flavour = (typeof FLAVOURS)[number];

// Browser-chrome colour per flavour: Catppuccin `crust`, matching the topbar.
// Keyed by the resolved flavour, so "system" is resolved first.
const CRUST: Record<Exclude<Flavour, "system">, string> = {
  latte: "#dce0e8",
  frappe: "#232634",
  macchiato: "#181926",
  mocha: "#11111b",
};

export function storedFlavour(): Flavour {
  const v = localStorage.getItem(THEME_KEY);
  return (FLAVOURS as readonly string[]).includes(v ?? "") ? (v as Flavour) : "system";
}

export function applyFlavour(f: Flavour) {
  const root = document.documentElement;
  if (f === "system") {
    root.removeAttribute("data-theme");
    localStorage.removeItem(THEME_KEY);
  } else {
    root.setAttribute("data-theme", f);
    localStorage.setItem(THEME_KEY, f);
  }
  const resolved =
    f !== "system" ? f : matchMedia("(prefers-color-scheme: dark)").matches ? "mocha" : "latte";
  document.querySelector('meta[name="theme-color"]')?.setAttribute("content", CRUST[resolved]);
}
