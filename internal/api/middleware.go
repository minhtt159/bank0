package api

import (
	"net/http"
	"net/url"
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

// csrfGuard is a same-origin check for the cookie-authenticated portal surface
// (operator console + admin JSON). On an unsafe method it rejects a request whose
// Origin (or Referer fallback) host differs from the request host. A request with
// NEITHER header is allowed: a non-browser client (curl, a server-side script, the
// test harness) carries no ambient session cookie, so it is not a CSRF vector —
// CSRF attacks come from browsers, which always attach Origin on a cross-site POST.
// Defense in depth on top of the session cookie's SameSite=Strict. The JWT client
// surface uses bearer tokens (not auto-sent cross-site) and is intentionally not
// guarded here so CORS dev flows keep working.
func (s *Server) csrfGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
		default:
			if !sameOrigin(r) {
				writeError(w, http.StatusForbidden, "forbidden", "cross-origin request rejected")
				return
			}
			next.ServeHTTP(w, r)
		}
	})
}

// sameOrigin reports whether a state-changing request comes from this host. A
// missing Origin and Referer counts as same-origin (non-browser caller).
func sameOrigin(r *http.Request) bool {
	src := r.Header.Get("Origin")
	if src == "" {
		src = r.Header.Get("Referer")
	}
	if src == "" {
		return true
	}
	u, err := url.Parse(src)
	return err == nil && u.Host == r.Host
}
