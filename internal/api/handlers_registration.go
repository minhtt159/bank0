package api

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"net/http"
	"strings"

	"github.com/minhtt159/bank0/internal/api/genclient"
	"github.com/minhtt159/bank0/internal/db"
	sqlc "github.com/minhtt159/bank0/internal/db/sqlc"
)

// Public self-registration & contact verification (spec-self-registration.md).
// All three endpoints are pre-auth (security: []) and registered on the parent
// router ahead of requireJWT, like /auth/login. The whole signup (idempotency
// gate + locked user + first challenge) is ONE DB call — register_user.

// newVerificationCode returns a 6-digit zero-padded numeric code (crypto/rand).
func newVerificationCode() string {
	n, _ := rand.Int(rand.Reader, big.NewInt(1_000_000))
	return fmt.Sprintf("%06d", n)
}

type registerReq struct {
	Username       string `json:"username"`
	Password       string `json:"password"`
	FullName       string `json:"full_name"`
	Email          string `json:"email"`
	PhoneNumber    string `json:"phone_number"`
	InvitationCode string `json:"invitation_code"`
}

// Register implements genclient.ServerInterface (POST /auth/register).
func (s *Server) Register(w http.ResponseWriter, r *http.Request, params genclient.RegisterParams) {
	var req registerReq
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	req.FullName = strings.TrimSpace(req.FullName)
	req.Email = strings.TrimSpace(req.Email)
	req.PhoneNumber = strings.ReplaceAll(strings.TrimSpace(req.PhoneNumber), " ", "")
	req.InvitationCode = strings.TrimSpace(req.InvitationCode)
	if req.Username == "" || req.FullName == "" {
		writeError(w, http.StatusUnprocessableEntity, "invalid_input", "username and full_name are required")
		return
	}
	// Fast-fail; the gate itself (existence/consumed/expired) lives in register_user.
	if req.InvitationCode == "" {
		writeError(w, http.StatusUnprocessableEntity, "invalid_input", "invitation code required")
		return
	}
	// Fast-fail; the policy itself lives in register_user (>= 12, like change_password).
	if len(req.Password) < 12 {
		writeError(w, http.StatusUnprocessableEntity, "weak_password", "password must be at least 12 characters")
		return
	}
	if req.Email == "" && req.PhoneNumber == "" {
		writeError(w, http.StatusUnprocessableEntity, "invalid_input", "at least one of email or phone_number is required")
		return
	}

	// Prefer email when both are given.
	channel, destination := "phone", req.PhoneNumber
	if req.Email != "" {
		channel, destination = "email", req.Email
	}
	verifyToken := newSessionToken()
	code := newVerificationCode()

	res, err := s.pg.RegisterUser(r.Context(), db.RegisterParams{
		IdempotencyKey: params.IdempotencyKey,
		Username:       req.Username,
		Password:       req.Password,
		FullName:       req.FullName,
		Email:          nilIfEmpty(req.Email),
		PhoneNumber:    nilIfEmpty(req.PhoneNumber),
		Channel:        channel,
		Destination:    destination,
		TokenHash:      hashToken(verifyToken),
		CodeHash:       hashToken(code),
		VerifyToken:    verifyToken,
		InvitationCode: req.InvitationCode,
	})
	if err != nil {
		s.mapDBError(w, r, err)
		return
	}
	// Dispatch out-of-band only for a fresh registration; a replay must not
	// re-send (and its code hash wouldn't match the stored challenge anyway).
	if !res.WasReplay {
		if err := s.notifier.SendVerification(r.Context(), channel, destination, code); err != nil {
			s.logFor(r.Context()).Error("verification dispatch failed", "err", err, "channel", channel)
		}
	} else {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	// res.Response is the stored idempotency response JSONB — echo it verbatim so
	// a replay is byte-identical to the original 201.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write(res.Response)
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

type verifyContactReq struct {
	VerifyToken string `json:"verify_token"`
	Code        string `json:"code"`
}

// VerifyContact implements genclient.ServerInterface (POST /auth/verify-contact).
func (s *Server) VerifyContact(w http.ResponseWriter, r *http.Request) {
	var req verifyContactReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.VerifyToken == "" || req.Code == "" {
		writeError(w, http.StatusUnprocessableEntity, "invalid_input", "verify_token and code are required")
		return
	}
	res, err := s.pg.VerifyContact(r.Context(), hashToken(req.VerifyToken), hashToken(req.Code))
	if err != nil {
		s.mapDBError(w, r, err) // P0001 'not found'->404, 28000->401, 23514->422
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user_id":           res.UserID,
		"onboarding_status": res.OnboardingStatus,
		"channel":           res.Channel,
		"login_ready":       res.LoginReady,
	})
}

type resendCodeReq struct {
	VerifyToken string `json:"verify_token"`
}

// ResendCode implements genclient.ServerInterface (POST /auth/resend-code).
// Always 202 unless cooldown-throttled: a missing/consumed token is a silent
// no-op so the endpoint can't be used to probe token validity.
func (s *Server) ResendCode(w http.ResponseWriter, r *http.Request) {
	var req resendCodeReq
	if !decodeJSON(w, r, &req) {
		return
	}
	ch, err := s.pg.Queries.ChallengeByToken(r.Context(), hashToken(req.VerifyToken))
	if err != nil || ch.Status != sqlc.VerificationStatusPending {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	code := newVerificationCode()
	// Same token (the row is refreshed in place), fresh code. Cooldown -> 53400 -> 429.
	if _, err := s.pg.Queries.CreateVerificationChallenge(r.Context(), sqlc.CreateVerificationChallengeParams{
		UserID:      ch.UserID,
		Channel:     ch.Channel,
		Destination: ch.Destination,
		TokenHash:   hashToken(req.VerifyToken),
		CodeHash:    hashToken(code),
	}); err != nil {
		s.mapDBError(w, r, err)
		return
	}
	if err := s.notifier.SendVerification(r.Context(), string(ch.Channel), ch.Destination, code); err != nil {
		s.logFor(r.Context()).Error("verification dispatch failed", "err", err, "channel", ch.Channel)
	}
	w.WriteHeader(http.StatusAccepted)
}
