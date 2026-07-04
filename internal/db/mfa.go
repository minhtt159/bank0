package db

import (
	"context"

	"github.com/google/uuid"
)

// TOTP MFA state (spec-step-up-mfa). Thin pgx wrappers over the mfa_* PL/pgSQL —
// the throttle/lockout policy and the credential state machine live in the DB;
// the OTP math and seed encryption live in the API layer.

func (p *Postgres) MFAEnabled(ctx context.Context, userID uuid.UUID) (bool, error) {
	var v bool
	err := p.Pool.QueryRow(ctx, `SELECT mfa_enabled($1)`, userID).Scan(&v)
	return v, err
}

func (p *Postgres) MFABeginEnroll(ctx context.Context, userID uuid.UUID, secretEnc []byte) (uuid.UUID, error) {
	var id uuid.UUID
	err := p.Pool.QueryRow(ctx, `SELECT mfa_begin_enroll($1, $2)`, userID, secretEnc).Scan(&id)
	return id, err
}

func (p *Postgres) MFAPendingSecret(ctx context.Context, userID uuid.UUID) ([]byte, error) {
	var v []byte
	err := p.Pool.QueryRow(ctx, `SELECT mfa_pending_secret($1)`, userID).Scan(&v)
	return v, err
}

func (p *Postgres) MFAConfirm(ctx context.Context, userID uuid.UUID, recoveryHashes []string) error {
	_, err := p.Pool.Exec(ctx, `SELECT mfa_confirm($1, $2)`, userID, recoveryHashes)
	return err
}

func (p *Postgres) MFAConfirmedSecret(ctx context.Context, userID uuid.UUID) ([]byte, error) {
	var v []byte
	err := p.Pool.QueryRow(ctx, `SELECT mfa_confirmed_secret($1)`, userID).Scan(&v)
	return v, err
}

func (p *Postgres) MFABurnRecoveryCode(ctx context.Context, userID uuid.UUID, codeHash string) (bool, error) {
	var v bool
	err := p.Pool.QueryRow(ctx, `SELECT mfa_burn_recovery_code($1, $2)`, userID, codeHash).Scan(&v)
	return v, err
}

// MFARecordAttempt appends an attempt; returns true when the user is now locked.
func (p *Postgres) MFARecordAttempt(ctx context.Context, userID uuid.UUID, succeeded bool, ip string, maxFail, windowSeconds int) (bool, error) {
	var locked bool
	err := p.Pool.QueryRow(ctx, `SELECT mfa_record_attempt($1, $2, $3, $4, $5)`,
		userID, succeeded, ip, maxFail, windowSeconds).Scan(&locked)
	return locked, err
}

func (p *Postgres) MFAIsLocked(ctx context.Context, userID uuid.UUID, maxFail, windowSeconds int) (bool, error) {
	var v bool
	err := p.Pool.QueryRow(ctx, `SELECT mfa_is_locked($1, $2, $3)`, userID, maxFail, windowSeconds).Scan(&v)
	return v, err
}
