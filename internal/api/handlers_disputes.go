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
	ScamType string `json:"scam_type"` // optional PSR claim tag
}

func validScamType(t string) bool {
	switch sqlc.ScamType(t) {
	case sqlc.ScamTypeImpersonation, sqlc.ScamTypePurchase, sqlc.ScamTypeInvestment,
		sqlc.ScamTypeRomance, sqlc.ScamTypeInvoice, sqlc.ScamTypeAdvanceFee, sqlc.ScamTypeOther:
		return true
	}
	return false
}

// RaiseDispute implements genclient.ServerInterface ("I don't recognise this").
// Client surface only; the party check + disputability live in raise_dispute (the
// raiser owning a side is enforced in PL/pgSQL, not client-trusted).
func (s *Server) RaiseDispute(w http.ResponseWriter, r *http.Request, id openapi_types.UUID) {
	subj, ok := s.clientSubjectOr401(w, r)
	if !ok {
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
	rp := sqlc.RaiseDisputeParams{
		TransferID: uuid.UUID(id),
		Raiser:     subj,
		Category:   sqlc.DisputeCategory(req.Category),
		Reason:     req.Reason,
	}
	if req.ScamType != "" {
		if !validScamType(req.ScamType) {
			writeError(w, http.StatusUnprocessableEntity, "invalid_scam_type", "unknown scam type")
			return
		}
		rp.ScamType = &req.ScamType
	}
	disputeID, err := s.pg.Queries.RaiseDispute(r.Context(), rp)
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
	subj, ok := s.clientSubjectOr401(w, r)
	if !ok {
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
	subj, ok := s.clientSubjectOr401(w, r)
	if !ok {
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

type decideDisputeReq struct {
	Decision              string `json:"decision"`
	ReimbursedAmountMinor *int64 `json:"reimbursed_amount_minor"`
	Vulnerable            *bool  `json:"vulnerable"`
	Note                  string `json:"note"`
}

// DecideDispute implements genadmin.ServerInterface — the PSR claim outcome.
// A reimbursement MOVES REAL MONEY (EXTERNAL_CLEARING -> victim account) inside
// decide_dispute, idempotency-keyed on the dispute id; policy (cap/excess/
// vulnerable waiver) lives in the DB.
func (s *Server) DecideDispute(w http.ResponseWriter, r *http.Request, id openapi_types.UUID) {
	op, ok := s.requireRole(w, r, canActOnMoney)
	if !ok {
		return
	}
	var req decideDisputeReq
	if !decodeJSON(w, r, &req) {
		return
	}
	switch sqlc.DisputeDecision(req.Decision) {
	case sqlc.DisputeDecisionReimbursed, sqlc.DisputeDecisionPartiallyReimbursed, sqlc.DisputeDecisionDeclined:
	default:
		writeError(w, http.StatusUnprocessableEntity, "invalid_decision", "decision must be reimbursed, partially_reimbursed or declined")
		return
	}
	// One DB call: decide_dispute returns the currency alongside the payout, so a
	// durably-posted reimbursement can never be misreported by a failed read-back.
	payout, currency, err := s.pg.DecideDispute(r.Context(), uuid.UUID(id), op.UserID,
		req.Decision, req.ReimbursedAmountMinor, req.Vulnerable, req.Note)
	if err != nil {
		s.mapDBError(w, r, err) // terminal -> 409, bad amount -> 422, unknown -> 404
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": id, "decision": req.Decision, "payout_minor": payout, "currency": currency,
	})
}

type recallDisputeReq struct {
	Status string `json:"status"`
	Reason string `json:"reason"`
}

// RecallDispute implements genadmin.ServerInterface — the SIMULATED interbank
// recall (pacs.004) state machine: none -> requested -> funds_returned|refused.
func (s *Server) RecallDispute(w http.ResponseWriter, r *http.Request, id openapi_types.UUID) {
	op, ok := s.requireRole(w, r, canActOnMoney)
	if !ok {
		return
	}
	var req recallDisputeReq
	if !decodeJSON(w, r, &req) {
		return
	}
	switch sqlc.RecallStatus(req.Status) {
	case sqlc.RecallStatusRequested, sqlc.RecallStatusFundsReturned, sqlc.RecallStatusRefused:
	default:
		writeError(w, http.StatusUnprocessableEntity, "invalid_status", "status must be requested, funds_returned or refused")
		return
	}
	st, err := s.pg.Queries.SetDisputeRecall(r.Context(), sqlc.SetDisputeRecallParams{
		DisputeID: uuid.UUID(id),
		Actor:     op.UserID,
		Status:    sqlc.RecallStatus(req.Status),
		Reason:    req.Reason,
	})
	if err != nil {
		s.mapDBError(w, r, err) // illegal transition -> 409, unknown -> 404
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "recall_status": st})
}
