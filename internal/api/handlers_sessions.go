package api

import (
	"net/http"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/google/uuid"

	"github.com/minhtt159/bank0/internal/api/genclient"
	sqlc "github.com/minhtt159/bank0/internal/db/sqlc"
)

// ListSessions implements genclient.ServerInterface: the caller's active refresh-token
// families (devices), newest activity first. When the caller presents their current
// refresh token via X-Refresh-Token, that family is flagged current:true. Client
// surface only; never returns any token material. See docs/specs/spec-sessions-devices.md.
func (s *Server) ListSessions(w http.ResponseWriter, r *http.Request, params genclient.ListSessionsParams) {
	subj, ok := clientSubject(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}
	sessions, err := s.pg.ListUserSessions(r.Context(), subj)
	if err != nil {
		mapDBError(w, err)
		return
	}
	if params.XRefreshToken != nil && *params.XRefreshToken != "" {
		if fam, found, ferr := s.pg.RefreshFamilyByToken(r.Context(), hashToken(string(*params.XRefreshToken))); ferr == nil && found {
			for i := range sessions {
				if sessions[i].FamilyID == fam {
					sessions[i].Current = true
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, sessions) // ListUserSessions returns a non-nil slice -> []
}

// RevokeSession implements genclient.ServerInterface: selective sign-out of one family
// owned by the caller. Idempotent (204 even if already gone); 404 if the family isn't
// the caller's (never confirms a foreign family exists). Revoking the current family
// signs THIS device out at its next refresh.
func (s *Server) RevokeSession(w http.ResponseWriter, r *http.Request, familyID openapi_types.UUID) {
	subj, ok := clientSubject(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}
	n, err := s.pg.RevokeUserFamily(r.Context(), subj, uuid.UUID(familyID))
	if err != nil {
		mapDBError(w, err)
		return
	}
	if n == 0 {
		// 0 means not-owned/nonexistent OR owned-but-already-revoked. Stay idempotent
		// (204) for the owner; 404 a family that isn't theirs.
		owned, err := s.pg.Queries.FamilyOwnedBy(r.Context(), sqlc.FamilyOwnedByParams{
			Family: uuid.UUID(familyID), Owner: subj,
		})
		if err != nil {
			mapDBError(w, err)
			return
		}
		if !owned {
			writeError(w, http.StatusNotFound, "not_found", "session not found")
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}
