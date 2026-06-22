import { token, refreshToken, setAuth, clearAuth, logout } from "../store/auth";
import type {
  Account,
  Beneficiary,
  Dispute,
  DisputeCategory,
  LedgerEntry,
  LoginResponse,
  ResolvedAccount,
  Session,
  Transfer,
  TransferListItem,
  TransferResult,
  TransferSuggestion,
  User,
} from "./types";

// All requests go to /api/* on our own origin; the Cloudflare Worker (prod) or
// Vite (dev) proxies them to the client API, so there is never a CORS hop.
const BASE = "/api";

export class ApiError extends Error {
  status: number;
  code: string;
  constructor(status: number, code: string, message: string) {
    super(message);
    this.status = status;
    this.code = code;
  }
}

interface Opts {
  body?: unknown;
  idempotencyKey?: string;
  headers?: Record<string, string>;
}

function buildInit(method: string, opts: Opts): RequestInit {
  const headers: Record<string, string> = { ...opts.headers };
  if (token.value) headers["Authorization"] = `Bearer ${token.value}`;
  if (opts.body !== undefined) headers["Content-Type"] = "application/json";
  // The same Idempotency-Key rides every retry of one attempt, so a transparent
  // token refresh can never double-post a transfer.
  if (opts.idempotencyKey) headers["Idempotency-Key"] = opts.idempotencyKey;
  return {
    method,
    headers,
    body: opts.body !== undefined ? JSON.stringify(opts.body) : undefined,
  };
}

// postJSON is a bare POST for the auth endpoints (refresh/logout): JSON body, no
// Authorization header, and crucially no 401-refresh loop — those endpoints manage
// tokens themselves, so they must not recurse through req(). Returns the raw Response.
function postJSON(path: string, body: unknown): Promise<Response> {
  return fetch(BASE + path, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
}

// Single-flight refresh: concurrent 401s share one /auth/refresh round-trip.
let refreshing: Promise<boolean> | null = null;

function tryRefresh(): Promise<boolean> {
  if (refreshing) return refreshing;
  refreshing = (async () => {
    const rt = refreshToken.value;
    if (!rt) return false;
    try {
      const resp = await postJSON("/auth/refresh", { refresh_token: rt });
      if (!resp.ok) return false;
      const d = (await resp.json()) as LoginResponse;
      setAuth({ token: d.token, userId: d.user_id, expiresAt: d.expires_at, refreshToken: d.refresh_token });
      return true;
    } catch {
      return false;
    } finally {
      refreshing = null;
    }
  })();
  return refreshing;
}

async function req<T>(method: string, path: string, opts: Opts = {}, retried = false): Promise<T> {
  const resp = await fetch(BASE + path, buildInit(method, opts));

  // A 401 on a protected route means the access token died: try a one-shot
  // transparent refresh, else sign out. Auth endpoints (login/refresh) fall
  // through so their real error ("invalid username or password") is surfaced.
  if (resp.status === 401 && !path.startsWith("/auth/")) {
    if (!retried && refreshToken.value && (await tryRefresh())) {
      return req<T>(method, path, opts, true);
    }
    logout();
    throw new ApiError(401, "unauthorized", "Your session expired — please sign in again.");
  }

  if (resp.status === 204) return undefined as T;

  const text = await resp.text();
  let data: any = null;
  if (text) {
    try {
      data = JSON.parse(text);
    } catch {
      // Non-JSON body (e.g. a proxy/gateway error page or the worker's 502).
      // Surface a clean ApiError instead of an unhandled SyntaxError.
      throw new ApiError(resp.status, "bad_response", resp.ok ? "Unexpected response from server." : resp.statusText);
    }
  }
  if (!resp.ok) {
    throw new ApiError(resp.status, data?.error ?? "error", data?.message ?? resp.statusText);
  }
  return data as T;
}

export const api = {
  login: (username: string, password: string) =>
    req<LoginResponse>("POST", "/auth/login", { body: { username, password } }),
  // Best-effort server-side revoke of the refresh token, then clear local state.
  logout: async () => {
    const rt = refreshToken.value;
    if (rt) {
      try {
        await postJSON("/auth/logout", { refresh_token: rt });
      } catch {
        /* ignore — clearing local state below is what matters */
      }
    }
    clearAuth();
    location.hash = "/login";
  },
  me: () => req<User>("GET", "/me"),
  accounts: (uid: string) => req<Account[]>("GET", `/users/${uid}/accounts`),
  account: (id: string) => req<Account>("GET", `/accounts/${id}`),
  ledger: (id: string, cursor?: string, limit = 25) => {
    const q = new URLSearchParams({ limit: String(limit) });
    if (cursor) q.set("cursor", cursor);
    return req<LedgerEntry[]>("GET", `/accounts/${id}/ledger?${q}`);
  },
  beneficiaries: () => req<Beneficiary[]>("GET", "/beneficiaries"),
  resolve: (iban: string) =>
    req<ResolvedAccount>("GET", `/beneficiaries/resolve?iban=${encodeURIComponent(iban)}`),
  addBeneficiary: (label: string, iban: string) =>
    req<Beneficiary>("POST", "/beneficiaries", { body: { label, iban } }),
  deleteBeneficiary: (id: string) => req<void>("DELETE", `/beneficiaries/${id}`),
  createTransfer: (
    body: { debit_account: string; credit_account: string; amount_minor: number; description?: string },
    idempotencyKey: string,
  ) => req<TransferResult>("POST", "/transfers", { body, idempotencyKey }),
  getTransfer: (id: string) => req<Transfer>("GET", `/transfers/${id}`),

  // Cross-account transfer history, newest first. Keyset paging: pass the last row's
  // requested_at as `cursor` AND its id as `cursor_id` (composite tie-break).
  // `direction` is caller-relative (out = caller debits, in = caller credits).
  listTransfers: (opts: {
    cursor?: string;
    cursorId?: string;
    direction?: "out" | "in";
    q?: string;
    limit?: number;
  } = {}) => {
    const p = new URLSearchParams({ limit: String(opts.limit ?? 25) });
    if (opts.cursor) p.set("cursor", opts.cursor);
    if (opts.cursorId) p.set("cursor_id", opts.cursorId);
    if (opts.direction) p.set("direction", opts.direction);
    if (opts.q?.trim()) p.set("q", opts.q.trim());
    return req<TransferListItem[]>("GET", `/transfers?${p}`);
  },

  // Guided-transfer "mule menu": { options: [...] } with up to 3 candidate accounts
  // (empty when none). Unwrapped to the options array; the caller picks one at
  // random, and [] is the signal to fall back to the customer's own account.
  transferSuggestions: (fromAccount?: string, amountMinor?: number) => {
    const p = new URLSearchParams();
    if (fromAccount) p.set("from_account", fromAccount);
    if (amountMinor != null) p.set("amount_minor", String(amountMinor));
    const qs = p.toString();
    return req<{ options: TransferSuggestion[] }>(
      "GET",
      `/transfers/suggestion${qs ? `?${qs}` : ""}`,
    ).then((r) => r.options ?? []);
  },

  // Disputes. Raising one is NOT a money move (no Idempotency-Key); body is optional.
  raiseDispute: (transferId: string, body: { category: DisputeCategory; reason?: string }) =>
    req<Dispute>("POST", `/transfers/${transferId}/dispute`, { body }),
  disputes: () => req<Dispute[]>("GET", "/disputes"),
  dispute: (id: string) => req<Dispute>("GET", `/disputes/${id}`),

  // Profile self-service. updateMe is a partial PATCH (absent field = unchanged).
  updateMe: (body: { full_name?: string; email?: string; phone_number?: string }) =>
    req<User>("PATCH", "/me", { body }),
  // changePassword passes the current refresh token so THIS session is spared from
  // the revoke-other-families sweep the server runs on success. Returns 204.
  changePassword: (currentPassword: string, newPassword: string) =>
    req<void>("POST", "/me/password", {
      body: {
        current_password: currentPassword,
        new_password: newPassword,
        refresh_token: refreshToken.value || undefined,
      },
    }),

  // Active sessions/devices. Presenting X-Refresh-Token flags the current family.
  sessions: () =>
    req<Session[]>("GET", "/me/sessions", {
      headers: refreshToken.value ? { "X-Refresh-Token": refreshToken.value } : undefined,
    }),
  revokeSession: (familyId: string) => req<void>("DELETE", `/me/sessions/${familyId}`),
};
