package api

import "net/http"

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
	userID, err := s.pg.RotateRefreshToken(r.Context(),
		hashToken(req.RefreshToken), hashToken(newRefresh),
		int(s.refreshTTL.Seconds()), int(s.refreshAbs.Seconds()), r.UserAgent(), clientIP(r))
	if err != nil {
		mapDBError(w, err)
		return
	}
	u, err := s.pg.Queries.GetUserByID(r.Context(), userID)
	if err != nil {
		mapDBError(w, err)
		return
	}
	token, exp, err := s.issueJWT(userID, string(u.Role), u.Username)
	if err != nil {
		s.log.Error("issue jwt", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user_id":       userID,
		"token":         token,
		"token_type":    "Bearer",
		"expires_at":    exp,
		"refresh_token": newRefresh,
	})
}

// Logout implements genclient.ServerInterface: revoke the presented refresh token
// (single session). Best-effort; always 204. Public.
func (s *Server) Logout(w http.ResponseWriter, r *http.Request) {
	var req refreshReq
	decodeOptionalJSON(r, &req)
	if req.RefreshToken != "" {
		if err := s.pg.RevokeRefreshToken(r.Context(), hashToken(req.RefreshToken)); err != nil {
			mapDBError(w, err)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// LogoutAll implements genclient.ServerInterface: revoke every refresh token for
// the caller (log out everywhere). Behind requireJWT (needs the subject).
func (s *Server) LogoutAll(w http.ResponseWriter, r *http.Request) {
	subj, ok := clientSubject(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}
	if _, err := s.pg.RevokeUserRefresh(r.Context(), subj); err != nil {
		mapDBError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
