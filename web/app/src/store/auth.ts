import { signal, computed } from "@preact/signals";

// Token handling (docs/07 + docs/08 §2): a short-lived access token plus a
// rotating refresh token. Both live in memory mirrored to sessionStorage (survive
// reload, clear on tab close). The future BFF moves the refresh token to an
// httpOnly cookie — a Worker-only change.

const KEY = "bank0.auth";

interface Saved {
  token: string;
  userId: string;
  expiresAt: string;
  refreshToken: string;
}

function load(): Saved | null {
  try {
    return JSON.parse(sessionStorage.getItem(KEY) ?? "null");
  } catch {
    return null;
  }
}

const init = load();

export const token = signal(init?.token ?? "");
export const userId = signal(init?.userId ?? "");
export const expiresAt = signal(init?.expiresAt ?? "");
export const refreshToken = signal(init?.refreshToken ?? "");
export const isAuthed = computed(() => token.value !== "");

export function setAuth(s: Saved): void {
  token.value = s.token;
  userId.value = s.userId;
  expiresAt.value = s.expiresAt;
  refreshToken.value = s.refreshToken;
  sessionStorage.setItem(KEY, JSON.stringify(s));
}

export function clearAuth(): void {
  token.value = "";
  userId.value = "";
  expiresAt.value = "";
  refreshToken.value = "";
  sessionStorage.removeItem(KEY);
}

export function logout(): void {
  clearAuth();
  location.hash = "/login";
}
