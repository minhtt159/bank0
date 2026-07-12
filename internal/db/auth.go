package db

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Login verifies credentials via check_user_credentials, which returns the user's
// id plus the JWT claims (role, username) so the handler can mint the access token
// without a second GetUserByID round trip (AUTH-1). Invalid credentials yield zero
// rows -> ok=false.
func (p *Postgres) Login(ctx context.Context, username, password string) (id uuid.UUID, role, uname string, ok bool, err error) {
	err = p.Pool.QueryRow(ctx,
		`SELECT user_id, role, username FROM check_user_credentials($1::citext, $2::text)`,
		username, password).Scan(&id, &role, &uname)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, "", "", false, nil
	}
	if err != nil {
		return uuid.Nil, "", "", false, err
	}
	return id, role, uname, true, nil
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

// --- client (api) refresh tokens (docs/06 §3) ---

// IssueRefreshToken opens a new token family at login and returns the family id.
// deviceLabel is an optional client hint shown in GET /me/sessions ("" -> NULL).
func (p *Postgres) IssueRefreshToken(ctx context.Context, userID uuid.UUID, tokenHash string, idleSeconds int, userAgent, ip, deviceLabel string) (uuid.UUID, error) {
	var family uuid.UUID
	err := p.Pool.QueryRow(ctx,
		`SELECT issue_refresh_token($1::uuid, $2::text, $3::int, $4::text, $5::text, $6::text)`,
		userID, tokenHash, idleSeconds, userAgent, ip, nilIfZero(deviceLabel),
	).Scan(&family)
	return family, err
}

// nilIfZero returns nil for a zero value (e.g. "" or uuid.Nil) so pgx binds SQL
// NULL rather than the zero literal.
func nilIfZero[T comparable](v T) any {
	var zero T
	if v == zero {
		return nil
	}
	return v
}

// Session is one active refresh-token family (a device/login) for GET /me/sessions.
// It never exposes any token material — only the opaque family id and login hints.
type Session struct {
	FamilyID    uuid.UUID `json:"family_id"`
	DeviceLabel *string   `json:"device_label,omitempty"`
	UserAgent   *string   `json:"user_agent,omitempty"`
	IP          *string   `json:"ip,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	LastSeenAt  time.Time `json:"last_seen_at"`
	Current     bool      `json:"current"`
}

// ListUserSessions returns the caller's active sessions, newest activity first.
// (list_user_sessions RETURNS TABLE, which sqlc can't expand — hand-written pgx.)
func (p *Postgres) ListUserSessions(ctx context.Context, userID uuid.UUID) ([]Session, error) {
	rows, err := p.Pool.Query(ctx,
		`SELECT family_id, device_label, user_agent, ip, created_at, last_seen_at
		   FROM list_user_sessions($1::uuid)`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Session{} // non-nil so an empty list marshals as [] not null
	for rows.Next() {
		var s Session
		if err := rows.Scan(&s.FamilyID, &s.DeviceLabel, &s.UserAgent, &s.IP, &s.CreatedAt, &s.LastSeenAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// RevokeUserFamily revokes one refresh family iff owned by userID. Returns the count
// of live tokens revoked; 0 => not found / not owned / already revoked (the handler
// disambiguates not-owned -> 404 with an ownership probe).
func (p *Postgres) RevokeUserFamily(ctx context.Context, userID, familyID uuid.UUID) (int, error) {
	var n int
	err := p.Pool.QueryRow(ctx,
		`SELECT revoke_refresh_family_scoped($1::uuid, $2::uuid)`, userID, familyID).Scan(&n)
	return n, err
}

// RotateRefreshToken consumes oldHash and mints newHash atomically. On reuse it
// raises 28000 (family revoked); on expiry/unknown it raises 28P01 — both mapped
// to 401 by the API. Returns the token's user id.
func (p *Postgres) RotateRefreshToken(ctx context.Context, oldHash, newHash string, idleSeconds, absoluteSeconds int, userAgent, ip string) (userID uuid.UUID, role, uname string, err error) {
	var family uuid.UUID
	err = p.Pool.QueryRow(ctx,
		`SELECT user_id, family_id, role, username FROM rotate_refresh_token($1::text, $2::text, $3::int, $4::int, $5::text, $6::text)`,
		oldHash, newHash, idleSeconds, absoluteSeconds, userAgent, ip,
	).Scan(&userID, &family, &role, &uname)
	if err != nil {
		// Reuse detected (28000): rotate only RAISEd (its own UPDATE would roll
		// back), so revoke the family here in a separate, committing statement.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "28000" {
			_, _ = p.Pool.Exec(ctx, `SELECT revoke_refresh_family($1::text)`, oldHash)
		}
		return uuid.Nil, "", "", err
	}
	return userID, role, uname, nil
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
		`SELECT revoke_user_refresh($1::uuid, NULL::uuid, 'forced')`, userID).Scan(&n)
	return n, err
}

// ChangePassword verifies the current password and stores the new hash (one
// statement; FOR UPDATE serializes concurrent changes). Wrong current password ->
// 28P01 (mapped 401); policy failure (len / == current) -> 23514 (mapped 422).
func (p *Postgres) ChangePassword(ctx context.Context, userID uuid.UUID, current, next string) error {
	_, err := p.Pool.Exec(ctx, `SELECT change_password($1::uuid, $2::text, $3::text)`, userID, current, next)
	return err
}

// RevokeUserRefreshExceptFamily revokes every live refresh family for the user
// except keepFamily (pass uuid.Nil to revoke all). Returns the count revoked.
func (p *Postgres) RevokeUserRefreshExceptFamily(ctx context.Context, userID, keepFamily uuid.UUID) (int, error) {
	var n int
	err := p.Pool.QueryRow(ctx,
		`SELECT revoke_user_refresh($1::uuid, $2::uuid, 'password_change')`, userID, nilIfZero(keepFamily)).Scan(&n)
	return n, err
}

// RefreshFamilyByToken returns the family_id of a still-live refresh token (by its
// hash). ok=false when the token is unknown or already revoked.
func (p *Postgres) RefreshFamilyByToken(ctx context.Context, tokenHash string) (uuid.UUID, bool, error) {
	var fam uuid.UUID
	err := p.Pool.QueryRow(ctx,
		`SELECT family_id FROM refresh_tokens WHERE id = $1 AND revoked_at IS NULL`, tokenHash).Scan(&fam)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, nil
	}
	return fam, err == nil, err
}
