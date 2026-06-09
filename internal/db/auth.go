package db

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Login verifies credentials via check_user_credentials, which returns NULL on
// failure (a NULL that the generated non-nullable signature can't scan), so it
// is hand-written with a nullable scan. ok=false means invalid credentials.
func (p *Postgres) Login(ctx context.Context, username, password string) (id uuid.UUID, ok bool, err error) {
	var got *uuid.UUID
	err = p.Pool.QueryRow(ctx,
		`SELECT check_user_credentials($1::citext, $2::text)`, username, password).Scan(&got)
	if err != nil {
		return uuid.Nil, false, err
	}
	if got == nil {
		return uuid.Nil, false, nil
	}
	return *got, true, nil
}

// SessionUser is the authenticated subject behind a portal session.
type SessionUser struct {
	UserID   uuid.UUID
	Username string
	Role     string
}

// ErrLoginDenied is returned when credentials are wrong, the account is not
// active, or the user is not staff. Callers show one generic message (no leak).
var ErrLoginDenied = errors.New("login denied")

// staffLoginSQLStates are the SQLSTATEs create_staff_session raises for the
// three denial reasons; all collapse to ErrLoginDenied.
func isLoginDenied(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "28P01", "28000", "42501":
			return true
		}
	}
	return false
}

// CreateStaffSession verifies credentials + staff role + active status and mints
// a session, all in one DB function. tokenHash is sha256(token) hex.
func (p *Postgres) CreateStaffSession(ctx context.Context, username, password, tokenHash string, idleSeconds int, userAgent, ip string) (SessionUser, error) {
	var su SessionUser
	err := p.Pool.QueryRow(ctx,
		`SELECT user_id, username, role FROM create_staff_session($1::citext, $2::text, $3::text, $4::int, $5::text, $6::text)`,
		username, password, tokenHash, idleSeconds, userAgent, ip,
	).Scan(&su.UserID, &su.Username, &su.Role)
	if err != nil {
		if isLoginDenied(err) {
			return SessionUser{}, ErrLoginDenied
		}
		return SessionUser{}, err
	}
	return su, nil
}

// ValidateSession returns the session's user iff live, sliding the idle timeout
// forward. ok=false means invalid/expired.
func (p *Postgres) ValidateSession(ctx context.Context, tokenHash string, idleSeconds int) (SessionUser, bool, error) {
	var su SessionUser
	err := p.Pool.QueryRow(ctx,
		`SELECT user_id, username, role FROM validate_session($1::text, $2::int)`,
		tokenHash, idleSeconds,
	).Scan(&su.UserID, &su.Username, &su.Role)
	if errors.Is(err, pgx.ErrNoRows) {
		return SessionUser{}, false, nil
	}
	if err != nil {
		return SessionUser{}, false, err
	}
	return su, true, nil
}

func (p *Postgres) RevokeSession(ctx context.Context, tokenHash string) error {
	_, err := p.Pool.Exec(ctx, `SELECT revoke_session($1::text)`, tokenHash)
	return err
}

// --- client (api) refresh tokens (docs/07 §4) ---

// IssueRefreshToken opens a new token family at login and returns the family id.
func (p *Postgres) IssueRefreshToken(ctx context.Context, userID uuid.UUID, tokenHash string, idleSeconds int, userAgent, ip string) (uuid.UUID, error) {
	var family uuid.UUID
	err := p.Pool.QueryRow(ctx,
		`SELECT issue_refresh_token($1::uuid, $2::text, $3::int, $4::text, $5::text)`,
		userID, tokenHash, idleSeconds, userAgent, ip,
	).Scan(&family)
	return family, err
}

// RotateRefreshToken consumes oldHash and mints newHash atomically. On reuse it
// raises 28000 (family revoked); on expiry/unknown it raises 28P01 — both mapped
// to 401 by the API. Returns the token's user id.
func (p *Postgres) RotateRefreshToken(ctx context.Context, oldHash, newHash string, idleSeconds, absoluteSeconds int, userAgent, ip string) (uuid.UUID, error) {
	var userID, family uuid.UUID
	err := p.Pool.QueryRow(ctx,
		`SELECT user_id, family_id FROM rotate_refresh_token($1::text, $2::text, $3::int, $4::int, $5::text, $6::text)`,
		oldHash, newHash, idleSeconds, absoluteSeconds, userAgent, ip,
	).Scan(&userID, &family)
	if err != nil {
		// Reuse detected (28000): rotate only RAISEd (its own UPDATE would roll
		// back), so revoke the family here in a separate, committing statement.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "28000" {
			_, _ = p.Pool.Exec(ctx, `SELECT revoke_refresh_family($1::text)`, oldHash)
		}
		return uuid.Nil, err
	}
	return userID, nil
}

// RevokeRefreshToken is single-session logout; idempotent (no error if unknown).
func (p *Postgres) RevokeRefreshToken(ctx context.Context, tokenHash string) error {
	_, err := p.Pool.Exec(ctx, `SELECT revoke_refresh_token($1::text, 'logout')`, tokenHash)
	return err
}

// RevokeUserRefresh revokes every live refresh token for a user (log out
// everywhere / operator force-revoke). Returns the count revoked.
func (p *Postgres) RevokeUserRefresh(ctx context.Context, userID uuid.UUID) (int, error) {
	var n int
	err := p.Pool.QueryRow(ctx,
		`SELECT revoke_user_refresh($1::uuid, 'forced')`, userID).Scan(&n)
	return n, err
}
