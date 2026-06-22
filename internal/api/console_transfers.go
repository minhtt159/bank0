package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/minhtt159/bank0/internal/db"
	sqlc "github.com/minhtt159/bank0/internal/db/sqlc"
	template "github.com/minhtt159/bank0/web/template"
)

// ---- transfers (history + pending-queue actions + drill-down) -----------

// consoleTransfers renders the Transfers panel (search box + results container).
func (s *Server) consoleTransfers(w http.ResponseWriter, r *http.Request) {
	s.html(w)
	_ = template.TransfersPanel().Render(r.Context(), w)
}

// consoleTransfersResults shows ALL transfers (history), newest first, filtered
// by ?q and paginated by ?cursor. Pending rows are actionable for staff.
func (s *Server) consoleTransfersResults(w http.ResponseWriter, r *http.Request) {
	rows, prev, next, canAct, err := s.transfersPage(r)
	if err != nil {
		http.Error(w, "transfers error", http.StatusInternalServerError)
		return
	}
	s.html(w)
	_ = template.TransferTable(rows, canAct, prev, next, "").Render(r.Context(), w)
}

// renderTransfers re-renders the full table (used after post/cancel; no cursor).
func (s *Server) renderTransfers(w http.ResponseWriter, r *http.Request, flash string) {
	rows, prev, next, canAct, err := s.transfersPage(r)
	if err != nil {
		http.Error(w, "transfers error", http.StatusInternalServerError)
		return
	}
	s.html(w)
	_ = template.TransferTable(rows, canAct, prev, next, flash).Render(r.Context(), w)
}

func (s *Server) transfersPage(r *http.Request) ([]sqlc.SearchTransfersRow, string, string, bool, error) {
	ctx := r.Context()
	q := searchQ(r)
	ts, cid := pageCursor(r)
	limit := s.consolePageLimit(r)
	rows, err := s.pg.Queries.SearchTransfers(ctx, sqlc.SearchTransfersParams{
		Q: q, Cursor: ts, CursorID: cid, PageLimit: limit + 1,
	})
	if err != nil {
		s.log.Error("list transfers", "err", err)
		return nil, "", "", false, err
	}
	rows, lastTs, lastID, hasMore := paginate(rows, limit, func(t sqlc.SearchTransfersRow) (time.Time, uuid.UUID) {
		return t.RequestedAt, t.ID
	})
	prev, next := pagerLinks(r, "/console/transfers/results", q, lastTs, lastID, hasMore)
	canAct := false
	if su, ok := userFromContext(ctx); ok {
		canAct = canActOnMoney(su.Role)
	}
	return rows, prev, next, canAct, nil
}

func (s *Server) consolePostTransfer(w http.ResponseWriter, r *http.Request) {
	su, id, ok := s.consoleActionContext(w, r)
	if !ok {
		return
	}
	if _, err := s.pg.Queries.PostTransfer(r.Context(), id); err != nil {
		s.renderTransfers(w, r, "Could not post: "+s.dbFlash(r, err))
		return
	}
	s.audit(r.Context(), su, "post_transfer", &id, nil)
	s.renderTransfers(w, r, "Transfer posted.")
}

func (s *Server) consoleCancelTransfer(w http.ResponseWriter, r *http.Request) {
	su, id, ok := s.consoleActionContext(w, r)
	if !ok {
		return
	}
	reason := "cancelled via console by " + su.Username
	if _, err := s.pg.Queries.CancelTransfer(r.Context(), sqlc.CancelTransferParams{ID: id, Reason: reason}); err != nil {
		s.renderTransfers(w, r, "Could not cancel: "+s.dbFlash(r, err))
		return
	}
	s.audit(r.Context(), su, "cancel_transfer", &id, map[string]any{"reason": reason})
	s.renderTransfers(w, r, "Transfer cancelled.")
}

// consoleActionContext extracts the session user + {id} and enforces the money
// role. It writes the response and returns ok=false on any problem.
func (s *Server) consoleActionContext(w http.ResponseWriter, r *http.Request) (db.SessionUser, uuid.UUID, bool) {
	u, ok := s.requireRole(w, r, canActOnMoney)
	if !ok {
		return db.SessionUser{}, uuid.Nil, false
	}
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid transfer id")
		return db.SessionUser{}, uuid.Nil, false
	}
	return u, id, true
}

func (s *Server) consoleTransferDetail(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid transfer id")
		return
	}
	s.renderTransferDetail(w, r, id, "")
}

func (s *Server) renderTransferDetail(w http.ResponseWriter, r *http.Request, id uuid.UUID, flash string) {
	ctx := r.Context()
	t, err := s.pg.Queries.GetTransferDetail(ctx, id)
	if err != nil {
		s.mapDBError(w, r, err)
		return
	}
	legs, err := s.pg.Queries.TransferLegs(ctx, id)
	if err != nil {
		s.mapDBError(w, r, err)
		return
	}
	holds, _ := s.pg.Queries.HoldForTransfer(ctx, id)
	role := ""
	if su, ok := userFromContext(ctx); ok {
		role = su.Role
	}
	canReverse := canApprove(role) && t.Status == sqlc.TransferStatusPosted && t.Kind != sqlc.TransferKindReversal
	s.html(w)
	_ = template.TransferDetail(t, legs, holds, canReverse, flash).Render(ctx, w)
}

func (s *Server) consoleReverse(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireRole(w, r, canApprove)
	if !ok {
		return
	}
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid transfer id")
		return
	}
	_ = r.ParseForm()
	key := strings.TrimSpace(r.PostFormValue("idempotency_key"))
	if key == "" {
		key = uuid.NewString()
	}
	reason := strings.TrimSpace(r.PostFormValue("reason"))
	revID, err := s.pg.Queries.ReverseTransfer(r.Context(), sqlc.ReverseTransferParams{
		ID: id, IdempotencyKey: key, Reason: reason,
	})
	if err != nil {
		s.renderTransferDetail(w, r, id, "Could not reverse: "+s.dbFlash(r, err))
		return
	}
	refresh(w)
	s.audit(r.Context(), actor, "reverse", &id, map[string]any{"reversal_id": revID.String(), "reason": reason})
	s.renderTransferDetail(w, r, id, "Reversed — inverse entries posted.")
}
