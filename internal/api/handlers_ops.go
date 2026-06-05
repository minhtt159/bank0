package api

import "net/http"

// Health implements both ServerInterfaces.
func (s *Server) Health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"version": s.cfg.App.Version,
	})
}

// Reconcile implements genadmin.ServerInterface. healthy=true => books balanced.
func (s *Server) Reconcile(w http.ResponseWriter, r *http.Request) {
	issues, err := s.pg.Reconcile(r.Context())
	if err != nil {
		mapDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"healthy": len(issues) == 0,
		"issues":  issues,
	})
}

// ExpireHolds implements genadmin.ServerInterface.
func (s *Server) ExpireHolds(w http.ResponseWriter, r *http.Request) {
	n, err := s.pg.Queries.ExpireHolds(r.Context())
	if err != nil {
		mapDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"expired": n})
}
