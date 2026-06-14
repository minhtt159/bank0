// Package api wires HTTP routes to thin handlers. Routes come from the
// contract (api/openapi.yaml) via oapi-codegen: *Server implements the generated
// genclient.ServerInterface and genadmin.ServerInterface, so any drift between
// the spec and the handlers is a compile error.
package api

import (
	"net/http"
	"os"
	"time"

	"github.com/gorilla/mux"
	"github.com/minhtt159/bank0/internal/api/genadmin"
	"github.com/minhtt159/bank0/internal/api/genclient"
	"github.com/minhtt159/bank0/internal/config"
	"github.com/minhtt159/bank0/internal/db"
	webstatic "github.com/minhtt159/bank0/web/static"
	"log/slog"
)

// compile-time proof the handlers satisfy both generated contracts.
var (
	_ genclient.ServerInterface = (*Server)(nil)
	_ genadmin.ServerInterface  = (*Server)(nil)
)

type Server struct {
	cfg       config.Config
	log       *slog.Logger
	pg        *db.Postgres
	spec       []byte        // raw openapi.yaml, served at /openapi.yaml
	jwtSecret  []byte        // HS256 signing key for the client surface
	jwtTTL     time.Duration // access-token lifetime
	refreshTTL time.Duration // refresh-token idle window (slid on rotate)
	refreshAbs time.Duration // refresh-token absolute cap per family

	loginLimiter   *rateLimiter // /auth/login per-IP backstop (nil = disabled)
	refreshLimiter *rateLimiter // /auth/refresh per-IP backstop (nil = disabled)

	metrics metrics // RED counters rendered at /metrics
}

func NewServer(cfg config.Config, log *slog.Logger, pg *db.Postgres) *Server {
	s := &Server{cfg: cfg, log: log, pg: pg, jwtTTL: cfg.Auth.JWTTTL,
		refreshTTL: cfg.Auth.RefreshTTL, refreshAbs: cfg.Auth.RefreshAbsoluteTTL}
	if s.jwtTTL <= 0 {
		s.jwtTTL = time.Hour
	}
	if s.refreshTTL <= 0 {
		s.refreshTTL = 720 * time.Hour
	}
	if s.refreshAbs <= 0 {
		s.refreshAbs = 2160 * time.Hour
	}
	if n := cfg.Server.RateLimitPerMin; n > 0 {
		s.loginLimiter = newRateLimiter(n, time.Minute)
		s.refreshLimiter = newRateLimiter(n*3, time.Minute) // refresh runs more often than login
	}
	if cfg.Auth.JWTSecret == "" {
		log.Warn("auth.jwt_secret is empty — using an insecure dev secret; set APP_AUTH_JWT_SECRET in production")
		s.jwtSecret = []byte(devJWTSecret)
	} else {
		s.jwtSecret = []byte(cfg.Auth.JWTSecret)
	}
	if b, err := os.ReadFile(cfg.Server.OpenAPISpecPath); err == nil {
		s.spec = b
	} else {
		log.Warn("openapi spec not found; /openapi.yaml will 404", "path", cfg.Server.OpenAPISpecPath, "err", err)
	}
	return s
}

func (s *Server) Router() http.Handler {
	r := mux.NewRouter()
	// Order: requestLogger is OUTERMOST (assigns the request id + logger to ctx, so
	// recoverer/handlers can correlate), then recover from panics, harden headers,
	// and bound each request's lifetime.
	r.Use(s.requestLogger)
	r.Use(s.recoverer)
	r.Use(s.securityHeaders)
	r.Use(s.timeout)

	// Public on every surface: liveness, readiness, metrics, API docs.
	r.HandleFunc("/health", s.Health).Methods(http.MethodGet)
	r.HandleFunc("/readyz", s.Ready).Methods(http.MethodGet)
	r.HandleFunc("/metrics", s.Metrics).Methods(http.MethodGet)
	r.HandleFunc("/openapi.yaml", s.handleOpenAPISpec).Methods(http.MethodGet)
	r.HandleFunc("/docs", s.handleDocs).Methods(http.MethodGet)

	mode := s.cfg.Server.Mode
	if mode == "" {
		mode = "all"
	}
	apiOn := mode == "api" || mode == "all"
	portalOn := mode == "portal" || mode == "all"

	if portalOn {
		// Public portal auth endpoints.
		r.HandleFunc("/login", s.consoleLoginForm).Methods(http.MethodGet)
		r.Handle("/login", s.csrfGuard(http.HandlerFunc(s.consoleLoginSubmit))).Methods(http.MethodPost)
		r.Handle("/logout", s.csrfGuard(http.HandlerFunc(s.consoleLogout))).Methods(http.MethodPost)
		// Embedded console assets (CSS/JS). Public: the login page is styled too.
		r.PathPrefix("/static/").Handler(http.StripPrefix("/static/",
			staticCache(http.FileServerFS(webstatic.FS)))).Methods(http.MethodGet)
		// Static admin route registered on the parent BEFORE the client subrouter,
		// so it isn't shadowed by the client's greedy /transfers/{id} in "all" mode.
		r.Handle("/transfers/pending", s.requireSession(http.HandlerFunc(s.listPendingJSON))).Methods(http.MethodGet)
	}
	if apiOn {
		// Public: login issues the JWT + refresh token; refresh/logout take the
		// refresh token itself (the access token may be expired). Registered on the
		// parent ahead of the JWT-guarded subrouter so they aren't shadowed.
		// (logout-all needs the subject, so it stays on the guarded subrouter.)
		r.Handle("/auth/login", s.rateLimit(s.loginLimiter, s.clientIP, http.HandlerFunc(s.Login))).Methods(http.MethodPost)
		r.Handle("/auth/refresh", s.rateLimit(s.refreshLimiter, s.clientIP, http.HandlerFunc(s.Refresh))).Methods(http.MethodPost)
		// logout shares the refresh limiter so the whole /auth/* surface has the
		// per-IP backstop (it consumes a refresh token and is reachable unauthenticated).
		r.Handle("/auth/logout", s.rateLimit(s.refreshLimiter, s.clientIP, http.HandlerFunc(s.Logout))).Methods(http.MethodPost)
		cr := r.PathPrefix("/").Subrouter()
		cr.Use(s.requireJWT)
		genclient.HandlerFromMux(s, cr)
	}
	if portalOn {
		// Everything else on the portal (admin JSON API + console) needs a session.
		pr := r.PathPrefix("/").Subrouter()
		pr.Use(s.requireSession)
		pr.Use(s.csrfGuard) // same-origin guard on cookie-authed mutations (CSRF)
		genadmin.HandlerFromMux(s, pr)
		s.registerConsole(pr)
	}
	s.log.Info("router built", "mode", mode)

	// Opt-in CORS for the client API surface (dev QoL; empty allowlist = disabled,
	// the production default). Wrapped OUTSIDE the mux so a preflight OPTIONS is
	// answered before routing (mux would 405 an OPTIONS with no matching method).
	if apiOn && len(s.cfg.Server.CORSOrigins) > 0 {
		s.log.Info("CORS enabled for client API", "origins", s.cfg.Server.CORSOrigins)
		return s.cors(r)
	}
	return r
}

// staticCache lets browsers cache the embedded console assets briefly (the
// embed FS has no modtimes, so there is nothing to revalidate against).
func staticCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=300")
		next.ServeHTTP(w, r)
	})
}

// limitOr clamps the optional ?limit to the configured default/bounds.
func (s *Server) limitOr(l *int) int32 {
	if l != nil && *l > 0 && *l <= 200 {
		return int32(*l)
	}
	return s.cfg.Server.DefaultPageLimit
}
