package api

import (
	"net/http"

	"github.com/gorilla/mux"
)

// registerConsole mounts the operator console (server-rendered Templ + HTMX).
// Only mounted in "portal"/"all" modes. See console_handlers.go.
func (s *Server) registerConsole(r *mux.Router) {
	r.HandleFunc("/", s.consoleDashboard).Methods(http.MethodGet)
	r.HandleFunc("/console/accounts", s.consoleAccounts).Methods(http.MethodGet)
	r.HandleFunc("/console/pending", s.consolePending).Methods(http.MethodGet)
	// Pending-queue actions (operator/admin only; auditor is read-only).
	r.HandleFunc("/console/transfers/{id}/post", s.consolePostTransfer).Methods(http.MethodPost)
	r.HandleFunc("/console/transfers/{id}/cancel", s.consoleCancelTransfer).Methods(http.MethodPost)
}
