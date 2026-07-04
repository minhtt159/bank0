package db

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// RegisterParams carries the public-signup inputs plus the pre-hashed challenge
// material (the DB never sees the plaintext code; the plaintext verify_token is
// passed only so the idempotency response can replay it — see 00011).
type RegisterParams struct {
	IdempotencyKey string
	Username       string
	Password       string
	FullName       string
	Email          *string
	PhoneNumber    *string
	Channel        string // 'email' | 'phone'
	Destination    string
	TokenHash      string
	CodeHash       string
	VerifyToken    string
}

// RegisterResult mirrors register_user's RETURNS TABLE. Response is the stored
// idempotency response JSONB (the handler echoes it verbatim on replay).
type RegisterResult struct {
	UserID    uuid.UUID
	WasReplay bool
	Response  []byte
}

// RegisterUser runs the whole signup atomically (idempotency-key gate + locked
// user + first verification challenge) via register_user. RETURNS TABLE ->
// hand-written pgx, per the Transfer/RotateRefreshToken convention.
func (p *Postgres) RegisterUser(ctx context.Context, a RegisterParams) (RegisterResult, error) {
	var r RegisterResult
	err := p.Pool.QueryRow(ctx,
		`SELECT user_id, was_replay, response
		   FROM register_user($1, $2::citext, $3, $4, $5::citext, $6, $7::verification_channel, $8, $9, $10, $11)`,
		a.IdempotencyKey, a.Username, a.Password, a.FullName, a.Email, a.PhoneNumber,
		a.Channel, a.Destination, a.TokenHash, a.CodeHash, a.VerifyToken).
		Scan(&r.UserID, &r.WasReplay, &r.Response)
	return r, err
}

// VerifyContactResult mirrors verify_contact's RETURNS TABLE.
type VerifyContactResult struct {
	UserID           uuid.UUID
	OnboardingStatus string
	Channel          string
	LoginReady       bool
}

// VerifyContact consumes a verification code against a pending challenge.
// Wrong/expired code raises 28000 (-> 401), attempt lockout raises 23514
// (-> 422), unknown token raises P0001 'not found' (-> 404).
//
// On a WRONG code the attempt is persisted via record_failed_verification in a
// SEPARATE statement: verify_contact's RAISE rolls back its own writes, so an
// in-function increment would never survive (the rotate_refresh_token /
// revoke_refresh_family pattern — see CLAUDE.md).
func (p *Postgres) VerifyContact(ctx context.Context, tokenHash, codeHash string) (VerifyContactResult, error) {
	var r VerifyContactResult
	err := p.Pool.QueryRow(ctx,
		`SELECT user_id, onboarding, channel, login_ready FROM verify_contact($1, $2)`,
		tokenHash, codeHash).
		Scan(&r.UserID, &r.OnboardingStatus, &r.Channel, &r.LoginReady)
	if isWrongVerificationCode(err) {
		// best-effort: the challenge may have been consumed/expired concurrently.
		_, _ = p.Pool.Exec(ctx, `SELECT record_failed_verification($1)`, tokenHash)
	}
	return r, err
}

// isWrongVerificationCode matches ONLY verify_contact's wrong-code raise (28000
// with its exact message) — not the expired/consumed 28000, which must not
// count as an attempt.
func isWrongVerificationCode(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "28000" && pgErr.Message == "invalid verification code"
}
