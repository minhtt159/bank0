package api

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/google/uuid"
)

// reqCtxKey scopes the per-request values (id + logger) stashed in the context by
// requestLogger. Distinct type from auth.go's ctxKey so the values can't collide.
type reqCtxKey int

const (
	reqIDKey reqCtxKey = iota
	reqLoggerKey
)

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// requestLogger is the OUTERMOST middleware: it assigns a request id (honoring an
// inbound X-Request-Id from the edge, else minting one), stashes the id + a derived
// logger in the context so handlers and the recoverer can correlate their logs to
// the access line, records RED metrics, and logs the request.
func (s *Server) requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		reqID := r.Header.Get("X-Request-Id")
		if reqID == "" {
			reqID = uuid.NewString()
		}
		w.Header().Set("X-Request-Id", reqID)
		l := s.log.With("request_id", reqID)
		ctx := context.WithValue(r.Context(), reqIDKey, reqID)
		ctx = context.WithValue(ctx, reqLoggerKey, l)
		r = r.WithContext(ctx)

		next.ServeHTTP(rec, r)

		dur := time.Since(start)
		s.metrics.observe(r.Method, routeTemplate(r), rec.status, dur)
		l.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"dur_ms", dur.Milliseconds(),
		)
	})
}

// logFor returns the request-scoped logger (carrying request_id) if one is in the
// context, else the base logger. Handlers should log via s.logFor(r.Context()) so
// their lines correlate with the access log and panic traces.
func (s *Server) logFor(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(reqLoggerKey).(*slog.Logger); ok {
		return l
	}
	return s.log
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.logFor(r.Context()).Error("panic", "err", rec, "path", r.URL.Path)
				writeError(w, http.StatusInternalServerError, "internal", "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// securityHeaders sets safe, high-value response headers on every surface. The
// customer PWA gets its full CSP from the Worker; the Go operator console (a
// higher-privilege HTML surface) previously shipped NONE. These are harmless on
// JSON API responses too. A stricter script-src CSP is intentionally omitted so
// the CDN-loaded htmx keeps working (self-hosting htmx + script-src lockdown is a
// separate follow-up); what's here is the anti-clickjacking / sniffing core.
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("Content-Security-Policy", "frame-ancestors 'none'; base-uri 'self'; object-src 'none'")
		next.ServeHTTP(w, r)
	})
}

// timeout bounds each request to cfg.Server.RequestTimeout. When the deadline
// fires the request context is canceled, which cancels the in-flight pgx query and
// releases its pool connection — so one stuck query can't pin a connection (the
// pool is small) and stall the whole instance. 0/negative disables it.
func (s *Server) timeout(next http.Handler) http.Handler {
	d := s.cfg.Server.RequestTimeout
	if d <= 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), d)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
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
