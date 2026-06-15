package db

import (
	"context"

	"github.com/google/uuid"
	sqlc "github.com/minhtt159/bank0/internal/db/sqlc"
)

// This file holds the hand-written pgx accessor for request_money_with_approval,
// the set-returning PL/pgSQL function sqlc cannot expand (RETURNS TABLE). It is
// kept out of bank.go/auth.go so it can land without churning the generated sqlc
// code (no query, no `sqlc generate`).

// MakerCheckerRequest mirrors request_money_with_approval()'s RETURNS TABLE: the
// staged PENDING transfer and the admin_actions approval-queue row it enqueued.
type MakerCheckerRequest struct {
	TransferID uuid.UUID `json:"transfer_id"`
	RequestID  uuid.UUID `json:"request_id"`
}

// RequestMoneyWithApproval stages an above-threshold console credit/withdrawal for
// maker-checker ATOMICALLY (MAKER-CHECKER-ATOMICITY): one DB function creates the
// PENDING transfer + hold (the same request_transfer path request_deposit/
// request_withdrawal use) AND inserts the 4-eyes approval row, in one transaction.
// Either both commit or neither does — no orphaned hold without a queue row.
//
// kind is the transfer_kind direction: TransferKindDeposit (money in, external ->
// account) or TransferKindWithdrawal (money out, account -> external, holds funds).
// Idempotent on idempotencyKey (request_transfer's gate).
func (p *Postgres) RequestMoneyWithApproval(
	ctx context.Context,
	maker uuid.UUID,
	idempotencyKey string,
	account uuid.UUID,
	amountMinor int64,
	kind sqlc.TransferKind,
	description string,
	detail []byte,
) (MakerCheckerRequest, error) {
	const q = `SELECT transfer_id, request_id
	           FROM request_money_with_approval($1::uuid, $2::text, $3::uuid, $4::bigint, $5::transfer_kind, $6::text, $7::jsonb)`
	if len(detail) == 0 {
		detail = []byte("{}")
	}
	var r MakerCheckerRequest
	err := p.Pool.QueryRow(ctx, q, maker, idempotencyKey, account, amountMinor, kind, description, detail).
		Scan(&r.TransferID, &r.RequestID)
	return r, err
}
