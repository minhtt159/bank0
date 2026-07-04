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

// Transfer runs the auto-post convenience function (request + post in one txn).
// Idempotent on idempotencyKey.
func (p *Postgres) Transfer(
	ctx context.Context,
	idempotencyKey string,
	debit, credit uuid.UUID,
	amountMinor int64,
	description string,
	kind sqlc.TransferKind,
) (TransferResult, error) {
	const q = `SELECT transfer_id, status, was_replay
	           FROM transfer($1::text, $2::uuid, $3::uuid, $4::bigint, $5::text, $6::transfer_kind)`
	var r TransferResult
	err := p.Pool.QueryRow(ctx, q, idempotencyKey, debit, credit, amountMinor, description, kind).
		Scan(&r.TransferID, &r.Status, &r.WasReplay)
	return r, err
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

// RequestTransfer creates a transfer in the `pending` state (places a hold but
// does not post). Used for deferred settlement and the maker-checker queue; an
// operator later posts or cancels it. Idempotent on idempotencyKey.
func (p *Postgres) RequestTransfer(
	ctx context.Context,
	idempotencyKey string,
	debit, credit uuid.UUID,
	amountMinor int64,
	description string,
	kind sqlc.TransferKind,
) (TransferResult, error) {
	const q = `SELECT transfer_id, status, was_replay
	           FROM request_transfer($1::text, $2::uuid, $3::uuid, $4::bigint, $5::text, $6::transfer_kind)`
	var r TransferResult
	err := p.Pool.QueryRow(ctx, q, idempotencyKey, debit, credit, amountMinor, description, kind).
		Scan(&r.TransferID, &r.Status, &r.WasReplay)
	return r, err
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
}

// ResolveAccountByIban looks up an active customer account by IBAN and computes
// the CoP verdict against the customer-typed nameHint (nil = 'unable'). The
// function RAISEs (mapped to 404) when no active account matches.
func (p *Postgres) ResolveAccountByIban(ctx context.Context, iban string, nameHint *string) (ResolvedAccount, error) {
	const q = `SELECT account_id, iban, owner_name_masked, match_result, reason_code,
	                  suggested_name, account_type, gate, checked_at
	           FROM resolve_account_by_iban($1::varchar, $2::text)`
	var a ResolvedAccount
	err := p.Pool.QueryRow(ctx, q, iban, nameHint).Scan(&a.AccountID, &a.Iban, &a.OwnerNameMasked,
		&a.MatchResult, &a.ReasonCode, &a.SuggestedName, &a.AccountType, &a.Gate, &a.CheckedAt)
	return a, err
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
