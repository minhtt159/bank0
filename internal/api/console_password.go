package api

import (
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/minhtt159/bank0/web/template"
)

// The operator's own password. /me/password is client-surface and admin JSON refuses
// password writes, so this is the only staff rotation path. change_password() keeps
// the policy; this only adds the confirm-field check. docs/05 §4.6a.

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

	// Confirm-field is UI; policy is the DB's.
	if strings.TrimSpace(next) != strings.TrimSpace(confirm) {
		render("The new password and its confirmation do not match.")
		return
	}
	if err := s.pg.ChangePassword(r.Context(), su.UserID, current, next); err != nil {
		render(s.dbFlash(r, err))
		return
	}

	// Rotating a shared default must log out whoever else holds it.
	if _, err := s.pg.RevokeUserRefreshExceptFamily(r.Context(), su.UserID, uuid.Nil); err != nil {
		s.log.Warn("revoke refresh families after password change", "err", err)
	}
	s.audit(r.Context(), su, "password.changed", nil, nil)
	render("Password changed.")
}
