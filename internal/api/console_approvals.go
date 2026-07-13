package api

import (
	"net/http"
	"time"

	"github.com/google/uuid"

	sqlc "github.com/minhtt159/bank0/internal/db/sqlc"
	template "github.com/minhtt159/bank0/web/template"
)

// ---- approvals (maker-checker) ------------------------------------------

func (s *Server) consoleApprovals(w http.ResponseWriter, r *http.Request) {
	role := ""
	if su, ok := userFromContext(r.Context()); ok {
		role = su.Role
	}
	s.html(w)
	_ = template.ApprovalsPanel(canApprove(role)).Render(r.Context(), w)
}

func (s *Server) renderApprovals(w http.ResponseWriter, r *http.Request, flash string) {
	ctx := r.Context()
	ts, cid := pageCursor(r)
	limit := s.consolePageLimit(r)
	rows, err := s.pg.Queries.ListPendingApprovals(ctx, sqlc.ListPendingApprovalsParams{
		Cursor: ts, CursorID: cid, PageLimit: limit + 1,
	})
	if err != nil {
		s.log.Error("list approvals", "err", err)
		http.Error(w, "approvals error", http.StatusInternalServerError)
		return
	}
	rows, lastTs, lastID, hasMore := paginate(rows, limit, func(a sqlc.ListPendingApprovalsRow) (time.Time, uuid.UUID) {
		return a.CreatedAt, a.ID
	})
	prev, next := pagerLinks(r, "/console/approvals/results", nil, lastTs, lastID, hasMore)
	canAct := false
	if su, ok := userFromContext(ctx); ok {
		canAct = canApprove(su.Role)
	}
	s.html(w)
	_ = template.ApprovalRows(rows, canAct, prev, next, flash).Render(ctx, w)
}

func (s *Server) consoleApprovalsResults(w http.ResponseWriter, r *http.Request) {
	s.renderApprovals(w, r, "")
}

// ---- screening queue (AML, Rec 25) --------------------------------------
//
// Watchlist-matched client payments are parked under_review and surface here as a
// second queue on the Approvals page. Approve/reject reuse the maker-checker
// endpoints (approve_request/reject_request widen to screening_hold rows: approve
// posts the under_review transfer, reject cancels it), so those handlers set
// HX-Trigger: bank0:refresh and this fragment re-fetches to drop the acted-on row.

func (s *Server) renderScreenings(w http.ResponseWriter, r *http.Request, flash string) {
	ctx := r.Context()
	ts, cid := pageCursor(r)
	limit := s.consolePageLimit(r)
	rows, err := s.pg.Queries.ListPendingScreenings(ctx, sqlc.ListPendingScreeningsParams{
		Cursor: ts, CursorID: cid, PageLimit: limit + 1,
	})
	if err != nil {
		s.log.Error("list screenings", "err", err)
		http.Error(w, "screening error", http.StatusInternalServerError)
		return
	}
	rows, lastTs, lastID, hasMore := paginate(rows, limit, func(a sqlc.ListPendingScreeningsRow) (time.Time, uuid.UUID) {
		return a.CreatedAt, a.ID
	})
	prev, next := pagerLinks(r, "/console/screenings/results", nil, lastTs, lastID, hasMore)
	canAct := false
	if su, ok := userFromContext(ctx); ok {
		canAct = canApprove(su.Role)
	}
	s.html(w)
	_ = template.ScreeningRows(rows, canAct, prev, next, flash).Render(ctx, w)
}

func (s *Server) consoleScreeningsResults(w http.ResponseWriter, r *http.Request) {
	s.renderScreenings(w, r, "")
}

func (s *Server) consoleApprove(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireRole(w, r, canApprove)
	if !ok {
		return
	}
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request id")
		return
	}
	tid, err := s.pg.Queries.ApproveRequest(r.Context(), sqlc.ApproveRequestParams{RequestID: id, Approver: actor.UserID})
	if err != nil {
		s.renderApprovals(w, r, "Could not approve: "+s.dbFlash(r, err))
		return
	}
	refresh(w)
	s.audit(r.Context(), actor, "approve", &tid, map[string]any{"request_id": id.String()})
	s.renderApprovals(w, r, "Approved and posted.")
}

func (s *Server) consoleReject(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireRole(w, r, canApprove)
	if !ok {
		return
	}
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request id")
		return
	}
	tid, err := s.pg.Queries.RejectRequest(r.Context(), sqlc.RejectRequestParams{
		RequestID: id, Approver: actor.UserID, Reason: "rejected via console by " + actor.Username,
	})
	if err != nil {
		s.renderApprovals(w, r, "Could not reject: "+s.dbFlash(r, err))
		return
	}
	refresh(w)
	s.audit(r.Context(), actor, "reject", &tid, map[string]any{"request_id": id.String()})
	s.renderApprovals(w, r, "Request rejected.")
}

// ---- limit requests (customer maker-checker) -----------------------------

func (s *Server) consoleLimitRequests(w http.ResponseWriter, r *http.Request) {
	role := ""
	if su, ok := userFromContext(r.Context()); ok {
		role = su.Role
	}
	s.html(w)
	_ = template.LimitRequestsPanel(canApprove(role)).Render(r.Context(), w)
}

func (s *Server) renderLimitRequests(w http.ResponseWriter, r *http.Request, flash string) {
	ctx := r.Context()
	ts, cid := pageCursor(r)
	limit := s.consolePageLimit(r)
	rows, err := s.pg.Queries.ListLimitRequests(ctx, sqlc.ListLimitRequestsParams{
		Cursor: ts, CursorID: cid, PageLimit: limit + 1,
	})
	if err != nil {
		s.log.Error("list limit requests", "err", err)
		http.Error(w, "limit requests error", http.StatusInternalServerError)
		return
	}
	rows, lastTs, lastID, hasMore := paginate(rows, limit, func(q sqlc.ListLimitRequestsRow) (time.Time, uuid.UUID) {
		return q.RequestedAt, q.RequestID
	})
	prev, next := pagerLinks(r, "/console/limit-requests/results", nil, lastTs, lastID, hasMore)
	canAct := false
	if su, ok := userFromContext(ctx); ok {
		canAct = canApprove(su.Role)
	}
	s.html(w)
	_ = template.LimitRequestRows(rows, canAct, prev, next, flash).Render(ctx, w)
}

func (s *Server) consoleLimitRequestsResults(w http.ResponseWriter, r *http.Request) {
	s.renderLimitRequests(w, r, "")
}

func (s *Server) consoleLimitApprove(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireRole(w, r, canApprove)
	if !ok {
		return
	}
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request id")
		return
	}
	acctID, err := s.pg.Queries.ApproveLimitChange(r.Context(), sqlc.ApproveLimitChangeParams{RequestID: id, Approver: actor.UserID})
	if err != nil {
		s.renderLimitRequests(w, r, "Could not apply: "+s.dbFlash(r, err))
		return
	}
	refresh(w)
	s.audit(r.Context(), actor, "limit_approve", &acctID, map[string]any{"request_id": id.String()})
	s.renderLimitRequests(w, r, "Limit applied.")
}

func (s *Server) consoleLimitReject(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireRole(w, r, canApprove)
	if !ok {
		return
	}
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request id")
		return
	}
	acctID, err := s.pg.Queries.RejectLimitChange(r.Context(), sqlc.RejectLimitChangeParams{
		RequestID: id, Approver: actor.UserID, Reason: "rejected via console by " + actor.Username,
	})
	if err != nil {
		s.renderLimitRequests(w, r, "Could not reject: "+s.dbFlash(r, err))
		return
	}
	refresh(w)
	s.audit(r.Context(), actor, "limit_reject", &acctID, map[string]any{"request_id": id.String()})
	s.renderLimitRequests(w, r, "Limit request rejected.")
}

// consoleActionContext extracts the session user + {id} and enforces the money
// role. It writes the response and returns ok=false on any problem.
