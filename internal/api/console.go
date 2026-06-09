package api

import (
	"net/http"

	"github.com/gorilla/mux"
)

// registerConsole mounts the operator console (server-rendered Templ + HTMX).
// Only mounted in "portal"/"all" modes, behind requireSession. Layout follows
// docs/04 §3: nav -> #main-panel, drill-down/forms -> #rail.
func (s *Server) registerConsole(r *mux.Router) {
	// Shell + main-panel screens (panels: search box + results container)
	r.HandleFunc("/", s.consoleHome).Methods(http.MethodGet)
	r.HandleFunc("/console/dashboard", s.consoleDashboard).Methods(http.MethodGet)
	r.HandleFunc("/console/users", s.consoleUsers).Methods(http.MethodGet)
	r.HandleFunc("/console/accounts", s.consoleAccounts).Methods(http.MethodGet)
	r.HandleFunc("/console/pending", s.consoleTransfers).Methods(http.MethodGet)
	r.HandleFunc("/console/reconcile", s.consoleReconcile).Methods(http.MethodGet)
	r.HandleFunc("/console/audit", s.consoleAudit).Methods(http.MethodGet)
	r.HandleFunc("/console/approvals", s.consoleApprovals).Methods(http.MethodGet)

	// Live-search results fragments (registered before /{id} so "results" wins)
	r.HandleFunc("/console/users/results", s.consoleUsersResults).Methods(http.MethodGet)
	r.HandleFunc("/console/accounts/results", s.consoleAccountsResults).Methods(http.MethodGet)
	r.HandleFunc("/console/transfers/results", s.consoleTransfersResults).Methods(http.MethodGet)
	r.HandleFunc("/console/audit/results", s.consoleAuditResults).Methods(http.MethodGet)
	r.HandleFunc("/console/approvals/results", s.consoleApprovalsResults).Methods(http.MethodGet)

	// Maker-checker approve/reject (admin only; approver must differ from maker)
	r.HandleFunc("/console/approvals/{id}/approve", s.consoleApprove).Methods(http.MethodPost)
	r.HandleFunc("/console/approvals/{id}/reject", s.consoleReject).Methods(http.MethodPost)

	// Users (admin-managed) + rail detail
	r.HandleFunc("/console/users/new", s.consoleNewUserForm).Methods(http.MethodGet)
	r.HandleFunc("/console/users", s.consoleCreateUser).Methods(http.MethodPost)
	r.HandleFunc("/console/users/{id}", s.consoleUserDetail).Methods(http.MethodGet)
	r.HandleFunc("/console/users/{id}", s.consoleUpdateUser).Methods(http.MethodPost)
	r.HandleFunc("/console/users/{id}/accounts", s.consoleCreateAccount).Methods(http.MethodPost)
	r.HandleFunc("/console/users/{id}/revoke-sessions", s.consoleRevokeSessions).Methods(http.MethodPost)

	// Account statement (drill-down, paginated) + money/admin actions
	r.HandleFunc("/console/accounts/{id}/statement", s.consoleStatement).Methods(http.MethodGet)
	r.HandleFunc("/console/accounts/{id}/credit", s.consoleCredit).Methods(http.MethodPost)
	r.HandleFunc("/console/accounts/{id}/withdraw", s.consoleWithdraw).Methods(http.MethodPost)
	r.HandleFunc("/console/accounts/{id}/status", s.consoleAccountStatus).Methods(http.MethodPost)
	r.HandleFunc("/console/accounts/{id}/limit", s.consoleAccountLimit).Methods(http.MethodPost)
	r.HandleFunc("/console/accounts/{id}/default", s.consoleAccountDefault).Methods(http.MethodPost)

	// Transfer detail (drill-down) + lifecycle actions
	r.HandleFunc("/console/transfers/{id}/detail", s.consoleTransferDetail).Methods(http.MethodGet)
	r.HandleFunc("/console/transfers/{id}/post", s.consolePostTransfer).Methods(http.MethodPost)
	r.HandleFunc("/console/transfers/{id}/cancel", s.consoleCancelTransfer).Methods(http.MethodPost)
	r.HandleFunc("/console/transfers/{id}/reverse", s.consoleReverse).Methods(http.MethodPost)
}
