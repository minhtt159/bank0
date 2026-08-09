package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"strings"

	"github.com/minhtt159/bank0/internal/db"
	template "github.com/minhtt159/bank0/web/template"
)

const sessionCookie = "bank0_session"

type ctxKey int

const userCtxKey ctxKey = iota

func newSessionToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func hashToken(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}

func userFromContext(ctx context.Context) (db.SessionUser, bool) {
	su, ok := ctx.Value(userCtxKey).(db.SessionUser)
	return su, ok
}

func (s *Server) idleSeconds() int {
	secs := int(s.cfg.Admin.SessionIdleTimeout.Seconds())
	if secs <= 0 {
		secs = 1800
	}
	return secs
}

// clientIP returns the client IP used for the per-IP rate-limit key (and audit).
// Forwarded headers are trusted ONLY when cfg.Server.TrustProxyHeaders is set —
// otherwise a caller could spoof X-Forwarded-For to get a fresh limiter bucket per
// request and defeat the credential-brute-force backstop, so we key on RemoteAddr.
//
// When trusted, CF-Connecting-IP wins: Cloudflare REPLACES it, so it is a single
// edge-authored value. X-Forwarded-For is read from the RIGHT instead —
// `trusted_proxy_hops` entries in — because a proxy running with use_remote_address
// (Envoy, nginx, Traefik) APPENDS the real downstream address rather than replacing
// the header. The left-most entry is therefore whatever the client sent: reading it
// let an attacker rotate the limiter key per request even with trust enabled. See
// docs/10 (RATELIMIT-XFF-SPOOF).
func (s *Server) clientIP(r *http.Request) string {
	if s.cfg.Server.TrustProxyHeaders {
		if cf := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); cf != "" {
			return cf
		}
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			hops := s.cfg.Server.TrustedProxyHops
			if hops < 1 {
				hops = 1
			}
			if hops > len(parts) {
				hops = len(parts) // fewer hops than configured: take the left-most we have
			}
			if ip := strings.TrimSpace(parts[len(parts)-hops]); ip != "" {
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) setSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.cfg.App.Env == "production",
		SameSite: http.SameSiteStrictMode,
		MaxAge:   s.idleSeconds(),
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   s.cfg.App.Env == "production",
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
}

// requireSession guards all portal routes (console HTML + admin JSON API). On a
// missing/expired session it redirects browsers/HTMX to /login and returns 401
// to programmatic (JSON) callers.
func (s *Server) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookie)
		if err != nil || c.Value == "" {
			s.denyAuth(w, r)
			return
		}
		su, ok, err := s.pg.ValidateSession(r.Context(), hashToken(c.Value), s.idleSeconds())
		if err != nil {
			s.log.Error("validate session", "err", err)
			writeError(w, http.StatusInternalServerError, "internal", "internal error")
			return
		}
		if !ok {
			s.clearSessionCookie(w)
			s.denyAuth(w, r)
			return
		}
		ctx := context.WithValue(r.Context(), userCtxKey, su)
		if !s.passwordRotationOK(w, r.WithContext(ctx), su) {
			return
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// passwordRotationOK holds a flagged staff member on the password screen (00018).
// The seeded password is published in this repo, so this is what makes "change it
// immediately" binding. docs/05 §4.6a.
func (s *Server) passwordRotationOK(w http.ResponseWriter, r *http.Request, su db.SessionUser) bool {
	switch r.URL.Path {
	case "/console/password", "/logout":
		return true
	}
	must, err := s.pg.MustChangePassword(r.Context(), su.UserID)
	if err != nil {
		// Fail OPEN: one failed SELECT must not lock every operator out.
		s.log.Error("must-change-password lookup", "err", err)
		return true
	}
	if !must {
		return true
	}
	switch { // same content negotiation as denyAuth
	case r.Header.Get("HX-Request") == "true":
		w.Header().Set("HX-Redirect", "/console/password")
		w.WriteHeader(http.StatusOK)
	case strings.Contains(r.Header.Get("Accept"), "text/html"):
		http.Redirect(w, r, "/console/password", http.StatusSeeOther)
	default:
		writeError(w, http.StatusForbidden, "password_change_required",
			"this account must change its password before using the portal (see /console/password)")
	}
	return false
}

func (s *Server) denyAuth(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Header.Get("HX-Request") == "true":
		w.Header().Set("HX-Redirect", "/login")
		w.WriteHeader(http.StatusOK)
	case strings.Contains(r.Header.Get("Accept"), "text/html"):
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	default:
		writeError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
	}
}

// --- console auth handlers ---

func (s *Server) consoleLoginForm(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	msg := ""
	if r.URL.Query().Get("changed") == "1" {
		msg = "Password changed — sign in again. All other sessions were signed out."
	}
	_ = template.LoginPage(msg).Render(r.Context(), w)
}

func (s *Server) consoleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid form")
		return
	}
	username := strings.TrimSpace(r.PostFormValue("username"))
	password := r.PostFormValue("password")
	token := newSessionToken()
	su, err := s.pg.CreateStaffSession(r.Context(), username, password, hashToken(token),
		s.idleSeconds(), r.UserAgent(), s.clientIP(r))
	if errors.Is(err, db.ErrLoginDenied) {
		s.pg.NoteFailedLogin(r.Context(), username)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		_ = template.LoginPage("Invalid credentials, or this account can't access the console.").Render(r.Context(), w)
		return
	}
	if err != nil {
		s.log.Error("create session", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	s.log.Info("login", "user", su.Username, "role", su.Role)
	s.audit(r.Context(), su, "login", nil, nil)
	s.setSessionCookie(w, token)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) consoleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		if err := s.pg.RevokeSession(r.Context(), hashToken(c.Value)); err != nil {
			s.log.Warn("revoke session", "err", err)
		}
	}
	s.clearSessionCookie(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}
