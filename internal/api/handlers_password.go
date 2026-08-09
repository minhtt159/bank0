package api

import (
	"net/http"

	"github.com/google/uuid"
)

type changePasswordReq struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
	RefreshToken    string `json:"refresh_token"`
}

// ChangePassword implements genclient.ServerInterface. Client surface only (behind
// requireJWT). It verifies the current password, stores the new one, and revokes
// every OTHER refresh-token family for the caller — the session performing the
// change is spared via its refresh_token's family_id, so the user isn't logged out
// of the device they're using. 204 on success. See
// docs/specs/spec-change-password.md.
func (s *Server) ChangePassword(w http.ResponseWriter, r *http.Request) {
	subj, ok := s.clientSubjectOr401(w, r)
	if !ok {
		return
	}
	var req changePasswordReq
	if !decodeJSON(w, r, &req) {
		return
	}
	// Friendly pre-check; assert_password_policy() is the authority.
	if len(req.NewPassword) < 12 {
		writeError(w, http.StatusUnprocessableEntity, "weak_password", "new password must be at least 12 characters")
		return
	}
	// bcrypt truncates at 72 bytes — a longer passphrase would silently keep only
	// its prefix.
	if len(req.NewPassword) > 72 {
		writeError(w, http.StatusUnprocessableEntity, "weak_password", "new password must be at most 72 bytes")
		return
	}
	if err := s.pg.ChangePassword(r.Context(), subj, req.CurrentPassword, req.NewPassword); err != nil {
		s.mapDBError(w, r, err) // 28P01 -> 401, 23514 -> 422
		return
	}
	// EVERY session, both surfaces, including the caller's: the old password may be
	// in someone else's hands and there is no way to tell which session is theirs.
	// The client re-authenticates with the new password.
	if _, err := s.pg.RevokeUserRefreshExceptFamily(r.Context(), subj, uuid.Nil); err != nil {
		// Password is already changed; log and still 204 rather than failing a
		// change that succeeded.
		s.log.Error("revoke families after password change", "err", err)
	}
	if _, err := s.pg.RevokeUserSessions(r.Context(), subj, ""); err != nil {
		s.log.Error("revoke sessions after password change", "err", err)
	}
	w.WriteHeader(http.StatusNoContent)
}
