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
  onboarding_status?: string;
  // Lifetime invitation budget still available to this user (drives /invite).
  invites_remaining?: number;
}

// Invitation-gated self-registration. The verification code itself is delivered
// out-of-band (email/SMS) and never appears in a response — only the opaque
// verify_token, which the client stashes to drive /auth/verify-contact.
export interface RegisterRequest {
  username: string;
  password: string;
  full_name: string;
  email?: string;
  phone_number?: string;
  invitation_code: string;
}

export interface RegisterResponse {
  user_id: string;
  onboarding_status: string;
  verify_channel: "email" | "phone";
  verify_token: string;
}

export interface VerifyContactRequest {
  verify_token: string;
  code: string;
}

export interface VerifyContactResponse {
  user_id: string;
  onboarding_status: string;
  channel: "email" | "phone";
  login_ready: boolean;
}

export interface ResendCodeRequest {
  verify_token: string;
}

export interface CreateInvitationResponse {
  code: string;
  expires_at: string;
  invites_remaining: number;
}

export type InvitationStatus = "pending" | "consumed" | "expired";

export interface Invitation {
  code: string;
  status: InvitationStatus;
  created_at: string;
  expires_at: string;
  consumed_at?: string | null;
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
  // Fraud-hold lifecycle (status "held" / "under_review"). Kept after release for
  // audit, so they may be present on posted/canceled transfers too.
  hold_reason?: string | null;
  hold_expires_at?: string | null;
}

// Read-only transfer preflight (POST /transfers/intent). Never moves money; it
// surfaces the fraud decision + optional warning card the client renders before
// the customer commits to sending.
export type TransferDecision = "allow" | "step_up" | "review" | "block" | "warn";
export type RiskBand = "low" | "medium" | "high";
export type WarningSeverity = "info" | "warning" | "critical";

export interface TransferWarning {
  warning_id: string;
  category: string;
  severity: WarningSeverity;
  headline: string;
  body: string;
  // true = the customer must tick "I understand" (and wait out the cooling-off)
  // before the server will accept the transfer.
  required_ack: boolean;
  // Seconds the ack must age before submit succeeds; 0 = no countdown.
  cooling_off_seconds: number;
}

export interface TransferIntent {
  decision: TransferDecision;
  risk_band: RiskBand;
  reason_codes: string[];
  warning?: TransferWarning | null;
  step_up_method?: string | null;
}

// CoP/VOP liability evidence (POST /me/warning-acks). Recorded BEFORE the transfer
// submit; the debit account must be the caller's. `acknowledged` defaults true
// (customer proceeded past the warning).
export interface WarningAckRequest {
  category: string;
  reason_code?: string;
  acknowledged?: boolean;
  debit_account_id?: string;
  counterparty_iban?: string;
  amount_minor?: number;
  device?: string;
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
