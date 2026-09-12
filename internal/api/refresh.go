package api

import (
	"net/http"

	"github.com/minhtt159/bank0/internal/db"
)

// writeTokenPair issues an access JWT for the user and writes the standard auth
// response (access token + the given refresh token). Login, Refresh and MfaVerify
// share it; amr records the factors this token proves (["pwd"] or ["pwd","otp"]).
// Returns false (after writing a 500) if the JWT can't be minted.
//
// A principal under forced rotation (00019) gets the access token — it is the
// credential /me/password needs — plus password_change_required, and no refresh
// token: the account is one password change away from re-authenticating anyway,
// and a possibly-compromised credential should not gain a long-lived one.
func (s *Server) writeTokenPair(w http.ResponseWriter, pr db.Principal, refresh string, amr []string, txnLink string) bool {
	token, exp, err := s.issueJWT(pr, amr, txnLink)
	if err != nil {
		s.log.Error("issue jwt", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return false
	}
	body := map[string]any{
		"user_id":    pr.UserID,
		"token":      token,
		"token_type": "Bearer",
		"expires_at": exp,
	}
	if pr.MustChangePassword {
		body["password_change_required"] = true
	} else {
		body["refresh_token"] = refresh
	}
	writeJSON(w, http.StatusOK, body)
	return true
}

// Refresh-token rotation for the client (api) surface (docs/06 §3). The refresh
// token is an opaque random string; the DB stores only sha256(token). Rotation
// is atomic with reuse detection in rotate_refresh_token().

type refreshReq struct {
	RefreshToken string `json:"refresh_token"`
}

// Refresh implements genclient.ServerInterface: rotate a refresh token for a new
// access + refresh pair. Public (the access token may already be expired).
func (s *Server) Refresh(w http.ResponseWriter, r *http.Request) {
	var req refreshReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.RefreshToken == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "refresh_token is required")
		return
	}
	newRefresh := newSessionToken()
	pr, err := s.pg.RotateRefreshToken(r.Context(),
		hashToken(req.RefreshToken), hashToken(newRefresh),
		int(s.refreshTTL.Seconds()), int(s.refreshAbs.Seconds()), r.UserAgent(), s.clientIP(r))
	if err != nil {
		s.mapDBError(w, r, err)
		return
	}
	// A refresh is not a re-authentication of the second factor: step-up
	// freshness is per-/auth/mfa/verify, never preserved across rotation.
	s.writeTokenPair(w, pr, newRefresh, []string{"pwd"}, "")
}

// Logout implements genclient.ServerInterface: revoke the presented refresh token
// (single session). Best-effort; always 204. Public.
func (s *Server) Logout(w http.ResponseWriter, r *http.Request) {
	var req refreshReq
	decodeOptionalJSON(r, &req)
	if req.RefreshToken != "" {
		if err := s.pg.RevokeRefreshToken(r.Context(), hashToken(req.RefreshToken)); err != nil {
			s.mapDBError(w, r, err)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// LogoutAll implements genclient.ServerInterface: revoke every refresh token for
// the caller (log out everywhere). Behind requireJWT (needs the subject).
func (s *Server) LogoutAll(w http.ResponseWriter, r *http.Request) {
	subj, ok := s.clientSubjectOr401(w, r)
	if !ok {
		return
	}
	if _, err := s.pg.RevokeUserRefresh(r.Context(), subj); err != nil {
		s.mapDBError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
