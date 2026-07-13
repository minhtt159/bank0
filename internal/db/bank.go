package db

import (
	"context"
	"time"

	"github.com/google/uuid"
	sqlc "github.com/minhtt159/bank0/internal/db/sqlc"
)

// These two calls hit set-returning PL/pgSQL functions (transfer(), reconcile())
// whose columns sqlc cannot expand, so they are hand-written with pgx.

// TransferResult mirrors the transfer() / request_transfer() RETURNS TABLE.
type TransferResult struct {
	TransferID uuid.UUID          `json:"transfer_id"`
	Status     sqlc.TransferStatus `json:"status"`
	WasReplay  bool               `json:"was_replay"`
}

// ClientTransfer is the client-surface auto-post transfer: client_transfer enforces
// (in the DB) that the caller owns the debit account, then runs transfer(). This
// replaces the handler's separate ownership-probe round trip (TRANSFER-1). Non-
// ownership raises 42501 -> 403 via mapDBError. endToEndID is the optional
// originator reference (ISO 20022 EndToEndId); nil/empty stores NULL.
func (p *Postgres) ClientTransfer(
	ctx context.Context,
	subject uuid.UUID,
	idempotencyKey string,
	debit, credit uuid.UUID,
	amountMinor int64,
	description string,
	endToEndID *string,
) (TransferResult, error) {
	const q = `SELECT transfer_id, status, was_replay
	           FROM client_transfer($1::uuid, $2::text, $3::uuid, $4::uuid, $5::bigint, $6::text, $7::varchar)`
	var r TransferResult
	err := p.Pool.QueryRow(ctx, q, subject, idempotencyKey, debit, credit, amountMinor, description, endToEndID).
		Scan(&r.TransferID, &r.Status, &r.WasReplay)
	return r, err
}

// OpenCustomerAccount opens a self-service account for the subject: server-minted
// ISO IBAN, bank_settings-sourced default limit + per-user cap, idempotent on the
// key (scope 'open_account'). RETURNS TABLE -> hand-written pgx.
func (p *Postgres) OpenCustomerAccount(ctx context.Context, idempotencyKey string, userID uuid.UUID) (accountID uuid.UUID, wasReplay bool, err error) {
	err = p.Pool.QueryRow(ctx,
		`SELECT account_id, was_replay FROM open_customer_account($1::text, $2::uuid)`,
		idempotencyKey, userID).Scan(&accountID, &wasReplay)
	return accountID, wasReplay, err
}

// ApprovalCheck is the maker-checker verdict for an amount (API-8): whether a
// second approver is required, plus the active threshold so callers can render it.
type ApprovalCheck struct {
	Required       bool
	ThresholdMinor int64
}

// RequiresApproval asks the DB whether an amount exceeds the configured maker-checker
// threshold. The decision + value live in bank_settings (tweakable from the console),
// honoring rule 1. requires_approval() RETURNS TABLE, so it's hand-written like
// transfer()/reconcile().
func (p *Postgres) RequiresApproval(ctx context.Context, amountMinor int64) (ApprovalCheck, error) {
	const q = `SELECT required, threshold_minor FROM requires_approval($1::bigint)`
	var a ApprovalCheck
	err := p.Pool.QueryRow(ctx, q, amountMinor).Scan(&a.Required, &a.ThresholdMinor)
	return a, err
}

// ResolvedAccount mirrors resolve_account_by_iban()'s RETURNS TABLE: the masked
// owner name plus the SERVER-SIDE CoP verdict (match/close_match/no_match/unable,
// gate, and — only on close_match — the actual registered name). Never the balance.
type ResolvedAccount struct {
	AccountID       uuid.UUID `json:"account_id"`
	Iban            string    `json:"iban"`
	OwnerNameMasked string    `json:"owner_name_masked"`
	MatchResult     string    `json:"match_result"`
	ReasonCode      string    `json:"reason_code"`
	SuggestedName   *string   `json:"suggested_name,omitempty"`
	AccountType     string    `json:"account_type"`
	Gate            string    `json:"gate"`
	CheckedAt       time.Time `json:"checked_at"`
	// Recipient-risk (Rec 11): server-decided badge + machine signals.
	RecipientRisk         string   `json:"recipient_risk"`
	MuleSuspected         bool     `json:"mule_suspected"`
	Signals               []string `json:"signals"`
	IsFirstPaymentToPayee bool     `json:"is_first_payment_to_payee"`
}

// ResolveAccountByIban looks up an active customer account by IBAN and computes
// the CoP verdict against the customer-typed nameHint (nil = 'unable') plus the
// recipient-risk signals for the caller (Rec 11). The function RAISEs (mapped
// to 404) when no active account matches.
func (p *Postgres) ResolveAccountByIban(ctx context.Context, iban string, nameHint *string, caller *uuid.UUID) (ResolvedAccount, error) {
	const q = `SELECT account_id, iban, owner_name_masked, match_result, reason_code,
	                  suggested_name, account_type, gate, checked_at,
	                  recipient_risk, mule_suspected, signals, is_first_payment_to_payee
	           FROM resolve_account_by_iban($1::varchar, $2::text, $3::uuid)`
	var a ResolvedAccount
	err := p.Pool.QueryRow(ctx, q, iban, nameHint, caller).Scan(&a.AccountID, &a.Iban, &a.OwnerNameMasked,
		&a.MatchResult, &a.ReasonCode, &a.SuggestedName, &a.AccountType, &a.Gate, &a.CheckedAt,
		&a.RecipientRisk, &a.MuleSuspected, &a.Signals, &a.IsFirstPaymentToPayee)
	return a, err
}

// RiskAssessment mirrors assess_transfer_risk()'s RETURNS TABLE (Rec 15 TRA seam).
type RiskAssessment struct {
	Band    string   `json:"risk_band"`
	Score   int32    `json:"score"`
	Reasons []string `json:"reasons"`
}

// AssessTransferRisk scores a transfer attempt server-side. Advisory client
// signals never feed it; the band ORs into the step-up gate's trigger set.
func (p *Postgres) AssessTransferRisk(ctx context.Context, caller, debit, credit uuid.UUID, amountMinor int64) (RiskAssessment, error) {
	var ra RiskAssessment
	err := p.Pool.QueryRow(ctx,
		`SELECT risk_band, score, reasons FROM assess_transfer_risk($1, $2, $3, $4)`,
		caller, debit, credit, amountMinor).Scan(&ra.Band, &ra.Score, &ra.Reasons)
	return ra, err
}

// TransferEvaluation mirrors evaluate_transfer()'s RETURNS TABLE (Rec 22 preflight):
// the single collapsed decision (allow|step_up|warn|review|block), the server-
// authoritative risk band + reason codes, and — when a warning_rule matched — the
// customer-facing warning copy and its ack/cooling-off policy. The numeric risk
// score is NEVER returned. RuleID/StepUpMethod are empty when not applicable.
type TransferEvaluation struct {
	Decision          string     `json:"decision"`
	RiskBand          string     `json:"risk_band"`
	ReasonCodes       []string   `json:"reason_codes"`
	RuleID            *uuid.UUID `json:"rule_id,omitempty"`
	Category          string     `json:"category"`
	Headline          string     `json:"headline"`
	Body              string     `json:"body"`
	Severity          string     `json:"severity"`
	RequiredAck       bool       `json:"required_ack"`
	CoolingOffSeconds int32      `json:"cooling_off_seconds"`
	StepUpMethod      string     `json:"step_up_method"`
}

// EvaluateTransfer runs the server-side preflight for a would-be transfer: it wraps
// evaluate_transfer (RETURNS TABLE -> sqlc can't expand it). The DB asserts the
// caller owns the debit account (42501 -> 403), so a foreign debit is rejected here,
// not in Go. stepUpLimitMinor is the configured per-payment step-up threshold (0
// disables the limit axis). exclude_transfer is not exposed: intent creates no row.
func (p *Postgres) EvaluateTransfer(
	ctx context.Context, subject, debit, credit uuid.UUID, amountMinor, stepUpLimitMinor int64,
) (TransferEvaluation, error) {
	const q = `SELECT decision, risk_band, reason_codes, rule_id, category, headline, body,
	                  severity, required_ack, cooling_off_seconds, step_up_method
	           FROM evaluate_transfer($1::uuid, $2::uuid, $3::uuid, $4::bigint, $5::bigint)`
	var e TransferEvaluation
	err := p.Pool.QueryRow(ctx, q, subject, debit, credit, amountMinor, stepUpLimitMinor).
		Scan(&e.Decision, &e.RiskBand, &e.ReasonCodes, &e.RuleID, &e.Category, &e.Headline,
			&e.Body, &e.Severity, &e.RequiredAck, &e.CoolingOffSeconds, &e.StepUpMethod)
	return e, err
}

// TransferSuggestion mirrors one row of suggest_transfer_destinations()'s RETURNS
// TABLE — a guided-transfer menu candidate (a third-party "mule"). Read-only; never
// exposes a full name or balance (mask_name, same masking as confirmation-of-payee).
// Source is always "scenario" from the backend (the operator-controlled mule
// short-list); Scenario is the matching scenario name.
type TransferSuggestion struct {
	AccountID       uuid.UUID `json:"account_id"`
	Iban            string    `json:"iban"`
	OwnerNameMasked string    `json:"owner_name_masked"`
	Reason          string    `json:"reason"`
	Scenario        *string   `json:"scenario,omitempty"`
	Source          string    `json:"source"`
}

// SuggestTransferDestinations resolves up to 3 candidate credit accounts for the
// guided-transfer menu: third-party accounts drawn at RANDOM from the ACTIVE
// guided_scenarios short-list (the mule targets — operator/seed-controlled, never
// arbitrary peers) that match the caller + amount and aren't the caller's own or the
// debit account. Returns an EMPTY slice when no mule is eligible — the client then
// falls back to the caller's own account. from may be nil (the resolver substitutes
// the caller's default account as the exclusion).
func (p *Postgres) SuggestTransferDestinations(
	ctx context.Context, caller uuid.UUID, from *uuid.UUID, amountMinor int64,
) ([]TransferSuggestion, error) {
	const q = `SELECT account_id, iban, owner_name_masked, reason, scenario, source
	           FROM suggest_transfer_destinations($1::uuid, $2::uuid, $3::bigint)`
	rows, err := p.Pool.Query(ctx, q, caller, from, amountMinor)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TransferSuggestion
	for rows.Next() {
		var sg TransferSuggestion
		if err := rows.Scan(&sg.AccountID, &sg.Iban, &sg.OwnerNameMasked, &sg.Reason, &sg.Scenario, &sg.Source); err != nil {
			return nil, err
		}
		out = append(out, sg)
	}
	return out, rows.Err()
}

// maintenanceLockKey is the advisory-lock key guarding periodic maintenance so
// that with many replicas only one actually runs the sweep per tick.
const maintenanceLockKey int64 = 912000001

// RunMaintenance runs expire_holds + cleanup, but only if it can grab the
// transaction-scoped advisory lock. ran=false means another replica is handling
// it this tick (not an error).
func (p *Postgres) RunMaintenance(ctx context.Context) (expired, cleaned, sessions, verifExpired, reconcileIssues int32, ran bool, err error) {
	tx, err := p.Pool.Begin(ctx)
	if err != nil {
		return 0, 0, 0, 0, 0, false, err
	}
	defer tx.Rollback(ctx)

	if err = tx.QueryRow(ctx, "SELECT pg_try_advisory_xact_lock($1)", maintenanceLockKey).Scan(&ran); err != nil {
		return 0, 0, 0, 0, 0, false, err
	}
	if !ran {
		return 0, 0, 0, 0, 0, false, nil
	}
	if err = tx.QueryRow(ctx, "SELECT expire_holds()").Scan(&expired); err != nil {
		return 0, 0, 0, 0, 0, false, err
	}
	if err = tx.QueryRow(ctx, "SELECT cleanup_idempotency_keys()").Scan(&cleaned); err != nil {
		return 0, 0, 0, 0, 0, false, err
	}
	if err = tx.QueryRow(ctx, "SELECT cleanup_sessions()").Scan(&sessions); err != nil {
		return 0, 0, 0, 0, 0, false, err
	}
	var refreshCleaned int32
	if err = tx.QueryRow(ctx, "SELECT cleanup_refresh_tokens()").Scan(&refreshCleaned); err != nil {
		return 0, 0, 0, 0, 0, false, err
	}
	if err = tx.QueryRow(ctx, "SELECT expire_verification_challenges()").Scan(&verifExpired); err != nil {
		return 0, 0, 0, 0, 0, false, err
	}
	var mfaCleaned int32
	if err = tx.QueryRow(ctx, "SELECT cleanup_mfa_attempts()").Scan(&mfaCleaned); err != nil {
		return 0, 0, 0, 0, 0, false, err
	}
	// Continuously assert the ledger/cache invariants (I1–I4): reconcile() returns
	// one row per drift. Running it on the maintenance tick turns the correctness
	// oracle from a manual spot-check into an automatic alarm, so a balance_minor /
	// held_minor cache divergence is caught without waiting for an operator to open
	// the Reconciliation panel. Read-only; shares the advisory lock + snapshot.
	if err = tx.QueryRow(ctx, "SELECT count(*)::int FROM reconcile()").Scan(&reconcileIssues); err != nil {
		return 0, 0, 0, 0, 0, false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, 0, 0, 0, 0, false, err
	}
	return expired, cleaned, sessions, verifExpired, reconcileIssues, true, nil
}

// ReconcileRow is one failing invariant from reconcile(). No rows => books balanced.
type ReconcileRow struct {
	CheckName string `json:"check_name"`
	Detail    string `json:"detail"`
}

// Reconcile asserts the correctness invariants (I1–I3). Empty slice = healthy.
func (p *Postgres) Reconcile(ctx context.Context) ([]ReconcileRow, error) {
	rows, err := p.Pool.Query(ctx, `SELECT check_name, detail FROM reconcile()`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ReconcileRow
	for rows.Next() {
		var r ReconcileRow
		if err := rows.Scan(&r.CheckName, &r.Detail); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
