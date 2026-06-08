import { token, logout } from "../store/auth";
import type {
  Account,
  Beneficiary,
  LedgerEntry,
  LoginResponse,
  ResolvedAccount,
  Transfer,
  TransferResult,
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
}

async function req<T>(method: string, path: string, opts: Opts = {}): Promise<T> {
  const headers: Record<string, string> = {};
  if (token.value) headers["Authorization"] = `Bearer ${token.value}`;
  if (opts.body !== undefined) headers["Content-Type"] = "application/json";
  if (opts.idempotencyKey) headers["Idempotency-Key"] = opts.idempotencyKey;

  const resp = await fetch(BASE + path, {
    method,
    headers,
    body: opts.body !== undefined ? JSON.stringify(opts.body) : undefined,
  });

  if (resp.status === 401) {
    logout();
    throw new ApiError(401, "unauthorized", "Your session expired — please sign in again.");
  }
  if (resp.status === 204) return undefined as T;

  const text = await resp.text();
  const data = text ? JSON.parse(text) : null;
  if (!resp.ok) {
    throw new ApiError(resp.status, data?.error ?? "error", data?.message ?? resp.statusText);
  }
  return data as T;
}

export const api = {
  login: (username: string, password: string) =>
    req<LoginResponse>("POST", "/auth/login", { body: { username, password } }),
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
};
