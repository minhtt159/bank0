package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"

	"github.com/google/uuid"
)

// TOTP MFA (spec-step-up-mfa). enroll/confirm sit behind requireJWT (you manage
// your own MFA while logged in); verify is public — the short-lived mfa_token
// (audience bank0-mfa) IS the credential. RFC 6238 defaults: SHA1 / 6 digits /
// 30s period, ±1-step drift window.

var totpOpts = totp.ValidateOpts{Period: 30, Skew: 1, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1}

func (s *Server) mfaUnavailable(w http.ResponseWriter) bool {
	if s.mfaConfigured() {
		return false
	}
	writeError(w, http.StatusServiceUnavailable, "mfa_unavailable", "MFA is not configured on this server")
	return true
}

// MfaEnroll implements genclient.ServerInterface (POST /auth/mfa/enroll).
func (s *Server) MfaEnroll(w http.ResponseWriter, r *http.Request) {
	if s.mfaUnavailable(w) {
		return
	}
	claims, ok := clientClaimsFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}
	subj, _ := clientSubject(r.Context())
	key, err := totp.Generate(totp.GenerateOpts{Issuer: "bank0", AccountName: claims.Username})
	if err != nil {
		s.logFor(r.Context()).Error("totp generate", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	enc, err := s.encryptSeed([]byte(key.Secret()))
	if err != nil {
		s.logFor(r.Context()).Error("seed encrypt", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	if _, err := s.pg.MFABeginEnroll(r.Context(), subj, enc); err != nil {
		s.mapDBError(w, r, err) // already enabled -> 23505 -> 409
		return
	}
	// The one and only time the raw secret leaves the server.
	writeJSON(w, http.StatusOK, map[string]any{"secret": key.Secret(), "otpauth_uri": key.URL()})
}

type mfaConfirmReq struct {
	Code string `json:"code"`
}

// MfaConfirm implements genclient.ServerInterface (POST /auth/mfa/confirm).
func (s *Server) MfaConfirm(w http.ResponseWriter, r *http.Request) {
	if s.mfaUnavailable(w) {
		return
	}
	subj, ok := clientSubject(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}
	var req mfaConfirmReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if locked, err := s.pg.MFAIsLocked(r.Context(), subj, s.cfg.Auth.MFALockMaxFail, int(s.cfg.Auth.MFALockWindow.Seconds())); err != nil {
		s.mapDBError(w, r, err)
		return
	} else if locked {
		writeError(w, http.StatusTooManyRequests, "mfa_locked", "too many failed attempts; try again later")
		return
	}
	enc, err := s.pg.MFAPendingSecret(r.Context(), subj)
	if err != nil {
		s.mapDBError(w, r, err) // no pending -> P0001 'no ... found' -> 404
		return
	}
	seed, err := s.decryptSeed(enc)
	if err != nil {
		s.logFor(r.Context()).Error("seed decrypt", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	valid, _ := totp.ValidateCustom(req.Code, string(seed), time.Now(), totpOpts)
	if locked, err := s.pg.MFARecordAttempt(r.Context(), subj, valid, s.clientIP(r),
		s.cfg.Auth.MFALockMaxFail, int(s.cfg.Auth.MFALockWindow.Seconds())); err != nil {
		s.mapDBError(w, r, err)
		return
	} else if !valid && locked {
		writeError(w, http.StatusTooManyRequests, "mfa_locked", "too many failed attempts; try again later")
		return
	}
	if !valid {
		writeError(w, http.StatusUnauthorized, "invalid_code", "the code did not match")
		return
	}
	codes := newRecoveryCodes(10)
	hashes := make([]string, len(codes))
	for i, c := range codes {
		hashes[i] = hashToken(c)
	}
	if err := s.pg.MFAConfirm(r.Context(), subj, hashes); err != nil {
		s.mapDBError(w, r, err)
		return
	}
	// Shown ONCE; stored hashed.
	writeJSON(w, http.StatusOK, map[string]any{"recovery_codes": codes})
}

type mfaVerifyReq struct {
	MfaToken string `json:"mfa_token"`
	Code     string `json:"code"`
	// Optional dynamic-linking commitment (PSD2 RTS Art. 5): binds THIS otp to
	// one payment. Required to pass the step-up gate; a login-time verify
	// (no link) yields a token that cannot authorize a gated transfer.
	Link *struct {
		DebitAccount  uuid.UUID `json:"debit_account"`
		CreditAccount uuid.UUID `json:"credit_account"`
		AmountMinor   int64     `json:"amount_minor"`
	} `json:"link"`
}

// MfaVerify implements genclient.ServerInterface (POST /auth/mfa/verify) —
// public: exchanges the pending-login mfa_token + a TOTP (or recovery code) for
// the real access + refresh pair, stamped amr=["pwd","otp"].
func (s *Server) MfaVerify(w http.ResponseWriter, r *http.Request) {
	if s.mfaUnavailable(w) {
		return
	}
	var req mfaVerifyReq
	if !decodeJSON(w, r, &req) {
		return
	}
	claims, err := s.parseMFAToken(req.MfaToken)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "invalid or expired mfa_token")
		return
	}
	userID, err := uuid.Parse(claims.Subject)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "invalid or expired mfa_token")
		return
	}
	if locked, err := s.pg.MFAIsLocked(r.Context(), userID, s.cfg.Auth.MFALockMaxFail, int(s.cfg.Auth.MFALockWindow.Seconds())); err != nil {
		s.mapDBError(w, r, err)
		return
	} else if locked {
		writeError(w, http.StatusTooManyRequests, "mfa_locked", "too many failed attempts; try again later")
		return
	}
	enc, err := s.pg.MFAConfirmedSecret(r.Context(), userID)
	if err != nil {
		s.mapDBError(w, r, err)
		return
	}
	seed, err := s.decryptSeed(enc)
	if err != nil {
		s.logFor(r.Context()).Error("seed decrypt", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	valid, _ := totp.ValidateCustom(req.Code, string(seed), time.Now(), totpOpts)
	if !valid {
		// Fallback: one-time recovery code.
		burned, err := s.pg.MFABurnRecoveryCode(r.Context(), userID, hashToken(req.Code))
		if err != nil {
			s.mapDBError(w, r, err)
			return
		}
		valid = burned
	}
	if locked, err := s.pg.MFARecordAttempt(r.Context(), userID, valid, s.clientIP(r),
		s.cfg.Auth.MFALockMaxFail, int(s.cfg.Auth.MFALockWindow.Seconds())); err != nil {
		s.mapDBError(w, r, err)
		return
	} else if !valid && locked {
		writeError(w, http.StatusTooManyRequests, "mfa_locked", "too many failed attempts; try again later")
		return
	}
	if !valid {
		writeError(w, http.StatusUnauthorized, "invalid_code", "the code did not match")
		return
	}

	txnLink := ""
	if req.Link != nil {
		txnLink = transferLinkHash(req.Link.DebitAccount, req.Link.CreditAccount, req.Link.AmountMinor)
	}
	refresh := newSessionToken()
	if _, err := s.pg.IssueRefreshToken(r.Context(), userID, hashToken(refresh),
		int(s.refreshTTL.Seconds()), r.UserAgent(), s.clientIP(r), ""); err != nil {
		s.mapDBError(w, r, err)
		return
	}
	s.writeTokenPair(w, userID, claims.Role, claims.Username, refresh, []string{"pwd", "otp"}, txnLink)
}

// transferLinkHash is the WYSIWYS commitment: the exact (debit, credit, amount)
// tuple the OTP authorizes — the same elements the idempotency fingerprint uses.
func transferLinkHash(debit, credit uuid.UUID, amountMinor int64) string {
	return hashToken(debit.String() + "|" + credit.String() + "|" + strconv.FormatInt(amountMinor, 10))
}
