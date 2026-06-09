// Mirrors the client-tagged schemas in api/openapi.yaml. Hand-kept small; could
// be generated with openapi-typescript later (docs/08 §6).

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
