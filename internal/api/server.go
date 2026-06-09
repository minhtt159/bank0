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
	r.Use(s.recoverer)
	r.Use(s.requestLogger)

	// Public on every surface: health probe + API docs.
	r.HandleFunc("/health", s.Health).Methods(http.MethodGet)
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
		r.HandleFunc("/login", s.consoleLoginSubmit).Methods(http.MethodPost)
		r.HandleFunc("/logout", s.consoleLogout).Methods(http.MethodPost)
		// Static admin route registered on the parent BEFORE the client subrouter,
		// so it isn't shadowed by the client's greedy /transfers/{id} in "all" mode.
		r.Handle("/transfers/pending", s.requireSession(http.HandlerFunc(s.listPendingJSON))).Methods(http.MethodGet)
	}
	if apiOn {
		// Public: login issues the JWT + refresh token; refresh/logout take the
		// refresh token itself (the access token may be expired). Registered on the
		// parent ahead of the JWT-guarded subrouter so they aren't shadowed.
		// (logout-all needs the subject, so it stays on the guarded subrouter.)
		r.HandleFunc("/auth/login", s.Login).Methods(http.MethodPost)
		r.HandleFunc("/auth/refresh", s.Refresh).Methods(http.MethodPost)
		r.HandleFunc("/auth/logout", s.Logout).Methods(http.MethodPost)
		cr := r.PathPrefix("/").Subrouter()
		cr.Use(s.requireJWT)
		genclient.HandlerFromMux(s, cr)
	}
	if portalOn {
		// Everything else on the portal (admin JSON API + console) needs a session.
		pr := r.PathPrefix("/").Subrouter()
		pr.Use(s.requireSession)
		genadmin.HandlerFromMux(s, pr)
		s.registerConsole(pr)
	}
	s.log.Info("router built", "mode", mode)
	return r
}

// limitOr clamps the optional ?limit to the configured default/bounds.
func (s *Server) limitOr(l *int) int32 {
	if l != nil && *l > 0 && *l <= 200 {
		return int32(*l)
	}
	return s.cfg.Server.DefaultPageLimit
}
