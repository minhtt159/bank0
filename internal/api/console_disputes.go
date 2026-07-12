package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	sqlc "github.com/minhtt159/bank0/internal/db/sqlc"
	template "github.com/minhtt159/bank0/web/template"
)

// ---- disputes (triage queue) --------------------------------------------

func (s *Server) consoleDisputes(w http.ResponseWriter, r *http.Request) {
	canAct := false
	if su, ok := userFromContext(r.Context()); ok {
		canAct = canActOnMoney(su.Role)
	}
	s.html(w)
	_ = template.DisputesPanel(canAct).Render(r.Context(), w)
}

func (s *Server) renderDisputes(w http.ResponseWriter, r *http.Request, flash string) {
	ctx := r.Context()
	ts, cid := pageCursor(r)
	limit := s.consolePageLimit(r)
	rows, err := s.pg.Queries.ListDisputesAdmin(ctx, sqlc.ListDisputesAdminParams{
		Cursor: ts, CursorID: cid, PageLimit: limit + 1, // status NULL => all
	})
	if err != nil {
		s.log.Error("list disputes", "err", err)
		http.Error(w, "disputes error", http.StatusInternalServerError)
		return
	}
	rows, lastTs, lastID, hasMore := paginate(rows, limit, func(d sqlc.ListDisputesAdminRow) (time.Time, uuid.UUID) {
		return d.CreatedAt, d.ID
	})
	prev, next := pagerLinks(r, "/console/disputes/results", nil, lastTs, lastID, hasMore)
	canAct := false
	if su, ok := userFromContext(ctx); ok {
		canAct = canActOnMoney(su.Role)
	}
	s.html(w)
	_ = template.DisputeRows(rows, canAct, prev, next, flash).Render(ctx, w)
}

func (s *Server) consoleDisputesResults(w http.ResponseWriter, r *http.Request) {
	s.renderDisputes(w, r, "")
}

// consoleResolveDispute drives resolve_dispute from the console. Gated to
// operators/admins (canActOnMoney) — matching the JSON admin handler; the DB
// function audits the transition in admin_actions. status comes from the query
// (?status=), the optional note from the form body.
func (s *Server) consoleResolveDispute(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireRole(w, r, canActOnMoney)
	if !ok {
		return
	}
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid dispute id")
		return
	}
	status := strings.TrimSpace(r.FormValue("status"))
	note := strings.TrimSpace(r.PostFormValue("resolution_note"))
	if !validResolveStatus(status) {
		s.renderDisputes(w, r, "Invalid status.")
		return
	}
	if _, err := s.pg.Queries.ResolveDispute(r.Context(), sqlc.ResolveDisputeParams{
		DisputeID: id, Resolver: actor.UserID, Status: sqlc.DisputeStatus(status), Note: note,
	}); err != nil {
		s.renderDisputes(w, r, "Could not resolve: "+s.dbFlash(r, err))
		return
	}
	refresh(w)
	s.renderDisputes(w, r, "Dispute "+status+".")
}
