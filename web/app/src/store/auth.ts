import { signal, computed } from "@preact/signals";

// MVP token handling (docs/08 §2): access token in memory, mirrored to
// sessionStorage so a reload survives but a closed tab clears it. The future
// BFF moves the refresh token to an httpOnly cookie — a Worker-only change.

const KEY = "bank0.auth";

interface Saved {
  token: string;
  userId: string;
  expiresAt: string;
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
export const isAuthed = computed(() => token.value !== "");

export function setAuth(s: Saved): void {
  token.value = s.token;
  userId.value = s.userId;
  expiresAt.value = s.expiresAt;
  sessionStorage.setItem(KEY, JSON.stringify(s));
}

export function logout(): void {
  token.value = "";
  userId.value = "";
  expiresAt.value = "";
  sessionStorage.removeItem(KEY);
  location.hash = "/login";
}
