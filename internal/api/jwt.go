package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
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
}

// hasFreshOTP reports whether the token proves a recent second factor. Step-up
// freshness is per-/auth/mfa/verify and deliberately NOT preserved across
// /auth/refresh — a rotated access token cannot satisfy a money move by itself.
func (c *clientClaims) hasFreshOTP(maxAge time.Duration) bool {
	for _, m := range c.AMR {
		if m == "otp" {
			return time.Since(time.Unix(c.AuthTime, 0)) <= maxAge
		}
	}
	return false
}

func (s *Server) issueJWT(userID uuid.UUID, role, username string, amr []string) (string, time.Time, error) {
	now := time.Now()
	exp := now.Add(s.jwtTTL)
	claims := clientClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID.String(),
			Issuer:    s.cfg.Auth.JWTIssuer,
			Audience:  jwt.ClaimStrings{s.cfg.Auth.JWTAudience},
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(exp),
		},
		Role:     role,
		Username: username,
		AMR:      amr,
		AuthTime: now.Unix(),
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
func (s *Server) issueMFAToken(userID uuid.UUID, role, username string) (string, error) {
	now := time.Now()
	claims := clientClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID.String(),
			Issuer:    s.cfg.Auth.JWTIssuer,
			Audience:  jwt.ClaimStrings{mfaTokenAudience},
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(s.cfg.Auth.MFATokenTTL)),
			ID:        uuid.NewString(),
		},
		Role:     role,
		Username: username,
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return tok.SignedString(s.jwtSecret)
}

func (s *Server) parseMFAToken(raw string) (*clientClaims, error) {
	claims := &clientClaims{}
	_, err := jwt.ParseWithClaims(raw, claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, errors.New("unexpected signing method")
		}
		return s.jwtSecret, nil
	},
		jwt.WithIssuer(s.cfg.Auth.JWTIssuer),
		jwt.WithAudience(mfaTokenAudience),
		jwt.WithExpirationRequired(),
		jwt.WithValidMethods([]string{"HS256"}),
	)
	if err != nil {
		return nil, err
	}
	return claims, nil
}

// clientClaimsFrom returns the full parsed claims for a client-surface request
// (the step-up gate needs amr/auth_time, not just the subject).
func clientClaimsFrom(ctx context.Context) (*clientClaims, bool) {
	c, ok := ctx.Value(subjectCtxKey).(*clientClaims)
	return c, ok
}

func (s *Server) parseJWT(raw string) (*clientClaims, error) {
	claims := &clientClaims{}
	_, err := jwt.ParseWithClaims(raw, claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, errors.New("unexpected signing method")
		}
		return s.jwtSecret, nil
	},
		jwt.WithIssuer(s.cfg.Auth.JWTIssuer),
		jwt.WithAudience(s.cfg.Auth.JWTAudience),
		jwt.WithExpirationRequired(),
		jwt.WithValidMethods([]string{"HS256"}),
	)
	if err != nil {
		return nil, err
	}
	return claims, nil
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
		ctx := context.WithValue(r.Context(), subjectCtxKey, claims)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
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

// ownsAccount returns true if the subject owns the (nullable) account owner.
func ownsAccount(subject uuid.UUID, owner *uuid.UUID) bool {
	return owner != nil && *owner == subject
}
