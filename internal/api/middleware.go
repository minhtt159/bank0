package api

import (
	"net/http"
	"time"

	"github.com/google/uuid"
)

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *Server) requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		reqID := uuid.NewString()
		w.Header().Set("X-Request-Id", reqID)

		next.ServeHTTP(rec, r)

		s.log.Info("request",
			"id", reqID,
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"dur_ms", time.Since(start).Milliseconds(),
		)
	})
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic", "err", rec, "path", r.URL.Path)
				writeError(w, http.StatusInternalServerError, "internal", "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// cors implements the opt-in dev CORS policy for the client API surface (config
// server.cors_origins; empty = disabled, the production default — prod web ships
// same-origin via the Worker). Exact-origin allowlist (no wildcard), no
// credentials (bearer header, no cookies). It answers the preflight OPTIONS itself
// so an unmatched method doesn't fall through to mux's 405; for that reason it is
// wrapped OUTSIDE the mux (see Router). Idempotency-Key is allow-listed or
// POST /transfers preflight would fail. See docs/09-fraudbank-bff-plan.md §1.2.
func (s *Server) cors(next http.Handler) http.Handler {
	allowed := s.cfg.Server.CORSOrigins
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" && originAllowed(allowed, origin) {
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Add("Vary", "Origin")
			h.Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
			h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Idempotency-Key")
			h.Set("Access-Control-Max-Age", "600")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func originAllowed(allowed []string, origin string) bool {
	for _, a := range allowed {
		if a == origin {
			return true
		}
	}
	return false
}
