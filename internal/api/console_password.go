package api

import (
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/minhtt159/bank0/web/template"
)

// The operator-facing half of password rotation. POST /me/password is the CLIENT
// surface (JWT + clientSubject), and the admin JSON surface refuses password writes
// on purpose — so before this screen existed, the seeded admin/admin bootstrap
// account (00016) had no supported way to rotate: psql was the only path.
//
// change_password() stays the sole authority (>= 12 chars, must differ, verifies the
// current one) and clears must_change_password itself.

func (s *Server) consolePasswordForm(w http.ResponseWriter, r *http.Request) {
	su, ok := userFromContext(r.Context())
	if !ok {
		s.denyAuth(w, r)
		return
	}
	must, err := s.pg.MustChangePassword(r.Context(), su.UserID)
	if err != nil {
		s.log.Error("must-change-password lookup", "err", err)
	}
	s.html(w)
	_ = template.PasswordScreen(su.Username, must, "").Render(r.Context(), w)
}

func (s *Server) consoleChangePassword(w http.ResponseWriter, r *http.Request) {
	su, ok := userFromContext(r.Context())
	if !ok {
		s.denyAuth(w, r)
		return
	}
	_ = r.ParseForm()
	current := r.PostFormValue("current_password")
	next := r.PostFormValue("new_password")
	confirm := r.PostFormValue("confirm_password")

	render := func(flash string) {
		must, _ := s.pg.MustChangePassword(r.Context(), su.UserID)
		s.html(w)
		_ = template.PasswordScreen(su.Username, must, flash).Render(r.Context(), w)
	}

	// Confirmation is a UI concern, so it is checked here; everything that is policy
	// (length, must-differ, current-password verification) belongs to the DB.
	if strings.TrimSpace(next) != strings.TrimSpace(confirm) {
		render("The new password and its confirmation do not match.")
		return
	}
	if err := s.pg.ChangePassword(r.Context(), su.UserID, current, next); err != nil {
		render(s.dbFlash(r, err))
		return
	}

	// Other sessions die with the old password — the point of rotating a shared
	// bootstrap credential is that anyone else holding it is logged out.
	if _, err := s.pg.RevokeUserRefreshExceptFamily(r.Context(), su.UserID, uuid.Nil); err != nil {
		s.log.Warn("revoke refresh families after password change", "err", err)
	}
	s.audit(r.Context(), su, "password.changed", nil, nil)
	render("Password changed.")
}
