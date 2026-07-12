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
	// Cheap policy pre-check for a friendly message; change_password() is the authority.
	if len(req.NewPassword) < 12 {
		writeError(w, http.StatusUnprocessableEntity, "weak_password", "new password must be at least 12 characters")
		return
	}
	if err := s.pg.ChangePassword(r.Context(), subj, req.CurrentPassword, req.NewPassword); err != nil {
		s.mapDBError(w, r, err) // 28P01 -> 401, 23514 -> 422
		return
	}
	// Spare the current session's family; revoke the rest. Best-effort: an unknown
	// or missing token revokes everything (the safer default).
	keep := uuid.Nil
	if req.RefreshToken != "" {
		if fam, found, ferr := s.pg.RefreshFamilyByToken(r.Context(), hashToken(req.RefreshToken)); ferr == nil && found {
			keep = fam
		}
	}
	if _, err := s.pg.RevokeUserRefreshExceptFamily(r.Context(), subj, keep); err != nil {
		// The password is already changed; log and still 204 (don't fail a succeeded change).
		s.log.Error("revoke other families after password change", "err", err)
	}
	w.WriteHeader(http.StatusNoContent)
}
