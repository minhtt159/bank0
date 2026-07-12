// Session handoff for the invitation-gated onboarding flow.
//
// The verify_token is an opaque credential for /auth/verify-contact — it must NOT
// ride in the URL (browser history, referrer leakage, shoulder-surfing), so it is
// stashed in sessionStorage (cleared when the tab closes) across the Register →
// Verify hop. A one-shot success notice is likewise handed to /login out of band.

const PENDING_KEY = "bank0:onboarding:pending";
const NOTICE_KEY = "bank0:onboarding:notice";

export interface PendingVerification {
  verify_token: string;
  channel: "email" | "phone";
}

export function stashPending(p: PendingVerification): void {
  try {
    sessionStorage.setItem(PENDING_KEY, JSON.stringify(p));
  } catch {
    /* private-mode / storage-disabled: Verify will bounce to /register */
  }
}

export function readPending(): PendingVerification | null {
  try {
    const raw = sessionStorage.getItem(PENDING_KEY);
    if (!raw) return null;
    const p = JSON.parse(raw) as PendingVerification;
    return p.verify_token ? p : null;
  } catch {
    return null;
  }
}

export function clearPending(): void {
  try {
    sessionStorage.removeItem(PENDING_KEY);
  } catch {
    /* ignore */
  }
}

// One-shot notice consumed by /login (e.g. "Your account is verified — sign in").
export function stashNotice(message: string): void {
  try {
    sessionStorage.setItem(NOTICE_KEY, message);
  } catch {
    /* ignore — the notice is a nicety, not load-bearing */
  }
}

export function takeNotice(): string {
  try {
    const n = sessionStorage.getItem(NOTICE_KEY);
    if (n) sessionStorage.removeItem(NOTICE_KEY);
    return n ?? "";
  } catch {
    return "";
  }
}
