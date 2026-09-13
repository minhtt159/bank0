package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/minhtt159/bank0/internal/db"
)

const devJWTSecret = "dev-insecure-secret-change-me"

type subjectCtxKeyT int

const subjectCtxKey subjectCtxKeyT = iota

// clientClaims is the JWT payload for the client (api) surface. sub = user id.
// amr + auth_time carry the authentication strength for the step-up gate
// (PSD2-style SCA): ["pwd"] from login/refresh, ["pwd","otp"] from
// /auth/mfa/verify. auth_time is the unix time of the FACTOR event.
type clientClaims struct {
	jwt.RegisteredClaims
	Role     string   `json:"role"`
	Username string   `json:"username"`
	AMR      []string `json:"amr,omitempty"`
	AuthTime int64    `json:"auth_time,omitempty"`
	// TxnLink dynamically links a step-up OTP to ONE payment (PSD2 RTS Art. 5 /
	// WYSIWYS): sha256(debit|credit|amount) committed at /auth/mfa/verify time.
	// Changing amount or payee invalidates the factor.
	TxnLink string `json:"txn_link,omitempty"`
	// PWC carries users.must_change_password (00019) from the function that
	// verified the credential. requireJWT holds such a token to the password
	// change; see passwordChangeRouteOK.
	PWC bool `json:"pwc,omitempty"`
}

// hasFreshOTP reports whether the token proves a recent second factor. Step-up
// freshness is per-/auth/mfa/verify and deliberately NOT preserved across
// /auth/refresh — a rotated access token cannot satisfy a money move by itself.
func (c *clientClaims) hasFreshOTP(maxAge time.Duration) bool {
	if slices.Contains(c.AMR, "otp") {
		return time.Since(time.Unix(c.AuthTime, 0)) <= maxAge
	}
	return false
}

func (s *Server) issueJWT(pr db.Principal, amr []string, txnLink string) (string, time.Time, error) {
	now := time.Now()
	exp := now.Add(s.jwtTTL)
	claims := clientClaims{
		Subject:   pr.UserID.String(),
		Issuer:    s.cfg.Auth.JWTIssuer,
		Audience:  jwt.ClaimStrings{s.cfg.Auth.JWTAudience},
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(exp),
		Role:      pr.Role,
		Username:  pr.Username,
		AMR:       amr,
		AuthTime:  now.Unix(),
		TxnLink:   txnLink,
		PWC:       pr.MustChangePassword,
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := tok.SignedString(s.jwtSecret)
	return signed, exp, err
}

// mfaTokenAudience isolates the short-lived pending-login token minted by
// /auth/login when MFA is required: it cannot be replayed against requireJWT
// (bank0-client) routes, and a client access token fails at /auth/mfa/verify.
const mfaTokenAudience = "bank0-mfa"

// issueMFAToken mints the pending-login token: same HS256 secret, distinct
// audience, short TTL. Carries role+username so verify can mint the real pair
// without a second user lookup.
func (s *Server) issueMFAToken(pr db.Principal) (string, error) {
	now := time.Now()
	claims := clientClaims{
		Subject:   pr.UserID.String(),
		Issuer:    s.cfg.Auth.JWTIssuer,
		Audience:  jwt.ClaimStrings{mfaTokenAudience},
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(now.Add(s.cfg.Auth.MFATokenTTL)),
		ID:        uuid.NewString(),
		Role:      pr.Role,
		Username:  pr.Username,
		PWC:       pr.MustChangePassword,
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return tok.SignedString(s.jwtSecret)
}

// parseWithAudience validates a token the one way this server ever validates one:
// HS256 with our secret, our issuer, an expiry, and the expected audience. The
// audience is the ONLY difference between a client access token and a pending-login
// MFA token, and it is what stops one being replayed as the other.
func (s *Server) parseWithAudience(raw, aud string) (*clientClaims, error) {
	claims := &clientClaims{}
	_, err := jwt.ParseWithClaims(raw, claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, errors.New("unexpected signing method")
		}
		return s.jwtSecret, nil
	},
		jwt.WithIssuer(s.cfg.Auth.JWTIssuer),
		jwt.WithAudience(aud),
		jwt.WithExpirationRequired(),
		jwt.WithValidMethods([]string{"HS256"}),
	)
	if err != nil {
		return nil, err
	}
	return claims, nil
}

func (s *Server) parseMFAToken(raw string) (*clientClaims, error) {
	return s.parseWithAudience(raw, mfaTokenAudience)
}

// clientClaimsFrom returns the full parsed claims for a client-surface request
// (the step-up gate needs amr/auth_time, not just the subject).
func clientClaimsFrom(ctx context.Context) (*clientClaims, bool) {
	c, ok := ctx.Value(subjectCtxKey).(*clientClaims)
	return c, ok
}

func (s *Server) parseJWT(raw string) (*clientClaims, error) {
	return s.parseWithAudience(raw, s.cfg.Auth.JWTAudience)
}

// requireJWT guards the client API surface. Missing/invalid bearer => 401.
func (s *Server) requireJWT(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := bearerToken(r)
		if raw == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing bearer token")
			return
		}
		claims, err := s.parseJWT(raw)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "invalid token")
			return
		}
		if claims.PWC && !passwordChangeRouteOK(r) {
			writeError(w, http.StatusForbidden, "password_change_required",
				"this account must change its password before using the API (POST /me/password)")
			return
		}
		ctx := context.WithValue(r.Context(), subjectCtxKey, claims)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// passwordChangeRouteOK is the allowlist a forced-rotation token may still reach:
// the change itself, and the logout that ends every other session. Anything else
// is 403 — without this the flag would brick the account instead of gating it.
func passwordChangeRouteOK(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}
	switch r.URL.Path {
	case "/me/password", "/auth/logout-all":
		return true
	}
	return false
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

// clientSubject returns the authenticated client subject (user id) if this is a
// client-surface (JWT) request. ok=false on the portal surface (cookie session),
// where ownership scoping does not apply (operators act on behalf of the bank).
func clientSubject(ctx context.Context) (uuid.UUID, bool) {
	c, ok := ctx.Value(subjectCtxKey).(*clientClaims)
	if !ok {
		return uuid.Nil, false
	}
	id, err := uuid.Parse(c.Subject)
	if err != nil {
		return uuid.Nil, false
	}
	return id, true
}

// clientSubjectOr401 returns the authenticated client subject, writing a 401 and
// returning ok=false when the request is not on the client (JWT) surface. Client
// handlers that require a subject before doing anything use it instead of repeating
// the clientSubject -> 401 preamble. Handlers that legitimately tolerate the portal
// surface (ok=false, e.g. the shared client+admin reads) keep calling clientSubject.
func (s *Server) clientSubjectOr401(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	subj, ok := clientSubject(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
	}
	return subj, ok
}

// ownsAccount returns true if the subject owns the (nullable) account owner.
func ownsAccount(subject uuid.UUID, owner *uuid.UUID) bool {
	return owner != nil && *owner == subject
}
