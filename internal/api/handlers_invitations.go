package api

import (
	"net/http"
	"time"
)

// Invitation-gated registration: a verified customer mints single-use codes
// (POST /me/invitations) that gate public signup, and lists their own invitations
// (GET /me/invitations). Both are client-surface, scoped to the JWT subject. The
// mint spends one unit of the caller's lifetime users.invites_remaining budget;
// all of that logic lives in create_invitation (rule 1).

// CreateInvitation implements genclient.ServerInterface (POST /me/invitations):
// mint one invitation for the caller. Not verified -> 403; budget exhausted -> 409.
func (s *Server) CreateInvitation(w http.ResponseWriter, r *http.Request) {
	subj, ok := s.clientSubjectOr401(w, r)
	if !ok {
		return
	}
	inv, err := s.pg.CreateInvitation(r.Context(), subj)
	if err != nil {
		s.mapDBError(w, r, err) // 42501->403, 23514 "invitation limit"->409, P0001->404
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"code":              inv.Code,
		"expires_at":        inv.ExpiresAt,
		"invites_remaining": inv.InvitesRemaining,
	})
}

// invitationView is the wire shape for a listed invitation; status is DERIVED from
// consumed_at/expires_at (never stored), consumed taking precedence over expired.
type invitationView struct {
	Code       string     `json:"code"`
	Status     string     `json:"status"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  time.Time  `json:"expires_at"`
	ConsumedAt *time.Time `json:"consumed_at"`
}

// ListInvitations implements genclient.ServerInterface (GET /me/invitations): the
// caller's invitations, newest first. Bare array (always [], never null).
func (s *Server) ListInvitations(w http.ResponseWriter, r *http.Request) {
	subj, ok := s.clientSubjectOr401(w, r)
	if !ok {
		return
	}
	rows, err := s.pg.Queries.ListInvitationsByInviter(r.Context(), subj)
	if err != nil {
		s.mapDBError(w, r, err)
		return
	}
	out := make([]invitationView, 0, len(rows))
	now := time.Now()
	for _, iv := range rows {
		status := "pending"
		switch {
		case iv.ConsumedAt != nil:
			status = "consumed"
		case iv.ExpiresAt.Before(now):
			status = "expired"
		}
		out = append(out, invitationView{
			Code:       iv.Code,
			Status:     status,
			CreatedAt:  iv.CreatedAt,
			ExpiresAt:  iv.ExpiresAt,
			ConsumedAt: iv.ConsumedAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}
