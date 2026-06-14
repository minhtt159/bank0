package api

import (
	"context"
	"net/http"
	"time"
)

// Health is the LIVENESS probe: a cheap, DB-blind "is the process up" check. It
// must NOT touch Postgres — a transient DB blip should not get the pod killed
// (that's readiness' job). Kubernetes liveness points here.
func (s *Server) Health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"version": s.cfg.App.Version,
	})
}

// Ready is the READINESS probe: it pings Postgres with a short deadline and
// returns 503 when the pool can't serve a connection, so a pod with a dead/
// exhausted pool is pulled from the Service rotation instead of silently serving
// 500s. The whole app is a thin shell over the DB, so "ready" means "DB reachable".
func (s *Server) Ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), time.Second)
	defer cancel()
	if err := s.pg.Pool.Ping(ctx); err != nil {
		s.logFor(r.Context()).Warn("readiness: db ping failed", "err", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ready"})
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
	if _, ok := s.requireRole(w, r, canActOnMoney); !ok {
		return
	}
	n, err := s.pg.Queries.ExpireHolds(r.Context())
	if err != nil {
		mapDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"expired": n})
}
