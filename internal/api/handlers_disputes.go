package api

import (
	"net/http"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/google/uuid"

	"github.com/minhtt159/bank0/internal/api/genadmin"
	"github.com/minhtt159/bank0/internal/api/genclient"
	sqlc "github.com/minhtt159/bank0/internal/db/sqlc"
)

func validDisputeCategory(c string) bool {
	switch sqlc.DisputeCategory(c) {
	case sqlc.DisputeCategoryUnrecognised, sqlc.DisputeCategoryFraud,
		sqlc.DisputeCategoryWrongAmount, sqlc.DisputeCategoryDuplicate, sqlc.DisputeCategoryOther:
		return true
	}
	return false
}

func validResolveStatus(s string) bool {
	switch sqlc.DisputeStatus(s) {
	case sqlc.DisputeStatusUnderReview, sqlc.DisputeStatusResolved, sqlc.DisputeStatusRejected:
		return true
	}
	return false
}

type raiseDisputeReq struct {
	Category string `json:"category"`
	Reason   string `json:"reason"`
}

// RaiseDispute implements genclient.ServerInterface ("I don't recognise this").
// Client surface only; the party check + disputability live in raise_dispute (the
// raiser owning a side is enforced in PL/pgSQL, not client-trusted).
func (s *Server) RaiseDispute(w http.ResponseWriter, r *http.Request, id openapi_types.UUID) {
	subj, ok := clientSubject(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}
	var req raiseDisputeReq
	decodeOptionalJSON(r, &req) // body optional (category/reason)
	if req.Category == "" {
		req.Category = string(sqlc.DisputeCategoryUnrecognised)
	}
	if !validDisputeCategory(req.Category) {
		writeError(w, http.StatusUnprocessableEntity, "invalid_category", "unknown dispute category")
		return
	}
	disputeID, err := s.pg.Queries.RaiseDispute(r.Context(), sqlc.RaiseDisputeParams{
		TransferID: uuid.UUID(id),
		Raiser:     subj,
		Category:   sqlc.DisputeCategory(req.Category),
		Reason:     req.Reason,
	})
	if err != nil {
		// not-a-party / unknown transfer -> 404; not disputable -> 422; dup open -> 409.
		s.mapDBError(w, r, err)
		return
	}
	d, err := s.pg.Queries.GetDisputeForRaiser(r.Context(), sqlc.GetDisputeForRaiserParams{
		ID:     disputeID,
		Raiser: subj,
	})
	if err != nil {
		s.mapDBError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, d)
}

// ListMyDisputes implements genclient.ServerInterface — subject-scoped, newest first.
func (s *Server) ListMyDisputes(w http.ResponseWriter, r *http.Request, params genclient.ListMyDisputesParams) {
	subj, ok := clientSubject(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}
	rows, err := s.pg.Queries.ListDisputesForRaiser(r.Context(), sqlc.ListDisputesForRaiserParams{
		Raiser:    subj,
		Cursor:    params.Cursor,
		PageLimit: s.limitOr(params.Limit),
	})
	if err != nil {
		s.mapDBError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rows) // writeJSON coerces a nil slice -> []
}

// GetDispute implements genclient.ServerInterface — scoped to the raiser. A dispute
// that exists but belongs to another user returns 404 (never revealed).
func (s *Server) GetDispute(w http.ResponseWriter, r *http.Request, id openapi_types.UUID) {
	subj, ok := clientSubject(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}
	d, err := s.pg.Queries.GetDisputeForRaiser(r.Context(), sqlc.GetDisputeForRaiserParams{
		ID:     uuid.UUID(id),
		Raiser: subj,
	})
	if err != nil {
		s.mapDBError(w, r, err) // ErrNoRows -> 404
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// ListDisputes implements genadmin.ServerInterface — the operator triage queue.
// Portal-session gated (requireSession), optional status filter + cursor.
func (s *Server) ListDisputes(w http.ResponseWriter, r *http.Request, params genadmin.ListDisputesParams) {
	var status sqlc.NullDisputeStatus
	if params.Status != nil {
		status = sqlc.NullDisputeStatus{DisputeStatus: sqlc.DisputeStatus(*params.Status), Valid: true}
	}
	rows, err := s.pg.Queries.ListDisputesAdmin(r.Context(), sqlc.ListDisputesAdminParams{
		Status:    status,
		Cursor:    params.Cursor,
		PageLimit: s.limitOr(params.Limit),
	})
	if err != nil {
		s.mapDBError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

type resolveDisputeReq struct {
	Status         string `json:"status"`
	ResolutionNote string `json:"resolution_note"`
}

// ResolveDispute implements genadmin.ServerInterface — the operator state machine.
// The resolver id is the session user (audited in admin_actions by resolve_dispute);
// illegal transitions -> 409, unknown id -> 404.
func (s *Server) ResolveDispute(w http.ResponseWriter, r *http.Request, id openapi_types.UUID) {
	op, ok := s.requireRole(w, r, canActOnMoney) // operators/admins resolve; auditors are read-only
	if !ok {
		return
	}
	var req resolveDisputeReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if !validResolveStatus(req.Status) {
		writeError(w, http.StatusUnprocessableEntity, "invalid_status",
			"status must be under_review, resolved, or rejected")
		return
	}
	if _, err := s.pg.Queries.ResolveDispute(r.Context(), sqlc.ResolveDisputeParams{
		DisputeID: uuid.UUID(id),
		Resolver:  op.UserID,
		Status:    sqlc.DisputeStatus(req.Status),
		Note:      req.ResolutionNote,
	}); err != nil {
		s.mapDBError(w, r, err) // unknown -> 404; illegal transition -> 409 invalid_state
		return
	}
	d, err := s.pg.Queries.GetDisputeAdmin(r.Context(), uuid.UUID(id))
	if err != nil {
		s.mapDBError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}
