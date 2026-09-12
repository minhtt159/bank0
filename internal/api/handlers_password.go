package api

import (
	"net/http"

	"github.com/google/uuid"
)

type changePasswordReq struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

// ChangePassword implements genclient.ServerInterface. Client surface only (behind
// requireJWT), and one of the two routes a forced-rotation token may still reach
// (jwt.go passwordChangeRouteOK). It verifies the current password, stores the new
// one, and revokes EVERY session and refresh family for the caller — including the
// one making the call, which must re-authenticate with the new password. 204 on
// success. docs/06 §2.
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
