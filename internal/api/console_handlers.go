package api

import (
	"net/http"

	"github.com/google/uuid"
	"github.com/gorilla/mux"

	"github.com/minhtt159/bank0/internal/db"
	sqlc "github.com/minhtt159/bank0/internal/db/sqlc"
	template "github.com/minhtt159/bank0/web/template"
)

// The operator console (Templ + HTMX), mounted in portal/all modes behind
// requireSession. It queries the DB directly (server-rendered) rather than going
// through the JSON API.

// canActOnMoney reports whether a role may post/cancel/move money. Auditors are
// read-only; customers can't reach the console at all.
func canActOnMoney(role string) bool {
	return role == string(sqlc.UserRoleOperator) || role == string(sqlc.UserRoleAdmin)
}

func (s *Server) consoleHTML(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
}

func (s *Server) consoleDashboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	stats, err := s.pg.Queries.DashboardStats(ctx)
	if err != nil {
		s.log.Error("dashboard stats", "err", err)
		http.Error(w, "dashboard error", http.StatusInternalServerError)
		return
	}
	issues, err := s.pg.Reconcile(ctx)
	if err != nil {
		s.log.Error("reconcile", "err", err)
		http.Error(w, "reconcile error", http.StatusInternalServerError)
		return
	}
	username := ""
	if su, ok := userFromContext(ctx); ok {
		username = su.Username
	}
	s.consoleHTML(w)
	_ = template.DashboardPage(stats, len(issues) == 0, issues, s.cfg.App.Version, username).Render(ctx, w)
}

func (s *Server) consoleAccounts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rows, err := s.pg.Queries.ListCustomerAccounts(ctx, s.cfg.Server.DefaultPageLimit)
	if err != nil {
		s.log.Error("list accounts", "err", err)
		http.Error(w, "accounts error", http.StatusInternalServerError)
		return
	}
	s.consoleHTML(w)
	_ = template.AccountsTable(rows).Render(ctx, w)
}

func (s *Server) consolePending(w http.ResponseWriter, r *http.Request) {
	s.renderPending(w, r, "")
}

// renderPending (re)renders the pending-transfer queue fragment, with an optional
// flash message shown above the table.
func (s *Server) renderPending(w http.ResponseWriter, r *http.Request, flash string) {
	ctx := r.Context()
	rows, err := s.pg.Queries.ListPendingEnriched(ctx, s.cfg.Server.DefaultPageLimit)
	if err != nil {
		s.log.Error("list pending", "err", err)
		http.Error(w, "pending error", http.StatusInternalServerError)
		return
	}
	canAct := false
	if su, ok := userFromContext(ctx); ok {
		canAct = canActOnMoney(su.Role)
	}
	s.consoleHTML(w)
	_ = template.PendingTable(rows, canAct, flash).Render(ctx, w)
}

func (s *Server) consolePostTransfer(w http.ResponseWriter, r *http.Request) {
	su, id, ok := s.consoleActionContext(w, r)
	if !ok {
		return
	}
	if _, err := s.pg.Queries.PostTransfer(r.Context(), id); err != nil {
		s.renderPending(w, r, "Could not post: "+dbErrorMessage(err))
		return
	}
	s.log.Info("console post_transfer", "by", su.Username, "transfer", id)
	s.renderPending(w, r, "Transfer posted.")
}

func (s *Server) consoleCancelTransfer(w http.ResponseWriter, r *http.Request) {
	su, id, ok := s.consoleActionContext(w, r)
	if !ok {
		return
	}
	_, err := s.pg.Queries.CancelTransfer(r.Context(), sqlc.CancelTransferParams{
		ID:     id,
		Reason: "cancelled via console by " + su.Username,
	})
	if err != nil {
		s.renderPending(w, r, "Could not cancel: "+dbErrorMessage(err))
		return
	}
	s.log.Info("console cancel_transfer", "by", su.Username, "transfer", id)
	s.renderPending(w, r, "Transfer cancelled.")
}

// consoleActionContext extracts the session user + {id} and enforces the money
// role. It writes the response and returns ok=false on any problem.
func (s *Server) consoleActionContext(w http.ResponseWriter, r *http.Request) (db.SessionUser, uuid.UUID, bool) {
	u, found := userFromContext(r.Context())
	if !found || !canActOnMoney(u.Role) {
		writeError(w, http.StatusForbidden, "forbidden", "your role cannot perform this action")
		return db.SessionUser{}, uuid.Nil, false
	}
	parsed, err := uuid.Parse(mux.Vars(r)["id"])
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid transfer id")
		return db.SessionUser{}, uuid.Nil, false
	}
	return u, parsed, true
}
