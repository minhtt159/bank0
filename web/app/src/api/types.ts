// Mirrors the client-tagged schemas in api/openapi.yaml. Hand-kept small; could
// be generated with openapi-typescript later (docs/07 §6).

export interface LoginResponse {
  user_id: string;
  token: string;
  token_type: string;
  expires_at: string;
  refresh_token: string;
}

export interface User {
  id: string;
  username: string;
  full_name: string;
  email?: string;
  phone_number?: string;
  role: string;
  status: string;
}

export interface Account {
  id: string;
  user_id: string;
  kind: string;
  iban?: string;
  currency: string;
  balance_minor: number;
  available_minor: number;
  transfer_limit_minor: number;
  is_default: boolean;
  status: string;
}

export interface LedgerEntry {
  id: string;
  direction: string;
  amount_minor: number;
  signed_amount: number;
  balance_after: number;
  currency: string;
  posted_at: string;
  description?: string;
  counterparty_iban?: string;
  counterparty_owner?: string;
}

export interface Beneficiary {
  id: string;
  label: string;
  credit_account_id: string;
  iban: string;
  owner_name_masked: string;
  created_at: string;
}

export interface ResolvedAccount {
  account_id: string;
  iban: string;
  owner_name_masked: string;
}

export interface TransferResult {
  transfer_id: string;
  status: string;
  was_replay: boolean;
}

export interface Transfer {
  id: string;
  debit_account_id: string;
  credit_account_id: string;
  amount_minor: number;
  currency: string;
  status: string;
  kind: string;
  description?: string;
  requested_at?: string;
  posted_at?: string;
}

// One transfer in the caller's cross-account history. `direction` is caller-relative:
// "out" = caller's account is the debit side, "in" = the credit side.
export interface TransferListItem {
  id: string;
  debit_account_id: string;
  credit_account_id: string;
  amount_minor: number;
  currency: string;
  status: string;
  kind: string;
  description?: string;
  direction: "out" | "in";
  counterparty_iban?: string;
  counterparty_owner?: string;
  requested_at?: string;
  posted_at?: string;
}

export type DisputeStatus = "open" | "under_review" | "resolved" | "rejected";
export type DisputeCategory = "unrecognised" | "fraud" | "wrong_amount" | "duplicate" | "other";

export interface Dispute {
  id: string;
  transfer_id: string;
  status: DisputeStatus;
  category: DisputeCategory;
  reason?: string;
  resolution_note?: string;
  created_at?: string;
  updated_at?: string;
}

// One active refresh-token family (a device/login). Carries no token material.
export interface Session {
  family_id: string;
  device_label?: string;
  user_agent?: string;
  ip?: string;
  created_at?: string;
  last_seen_at?: string;
  current?: boolean;
}

// Guided-transfer demo "mule menu" candidate. Read-only; never moves money. The
// endpoint returns { options: TransferSuggestion[] } (up to 3 third-party mule
// accounts). The backend always emits source "scenario"; "own_account" is
// synthesised client-side as the fallback when options is empty.
export interface TransferSuggestion {
  account_id: string;
  iban: string;
  owner_name_masked: string;
  reason?: string;
  scenario?: string;
  source: "scenario" | "own_account";
}
