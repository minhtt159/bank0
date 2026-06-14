package api

import (
	"net/http"

	sqlc "github.com/minhtt159/bank0/internal/db/sqlc"
	"github.com/minhtt159/bank0/internal/money"
	template "github.com/minhtt159/bank0/web/template"
)

// Operator-console Settings panel (API-8): bank policy lives in bank_settings, so
// it is tweakable here without a redeploy. All staff may view; admins may edit.

func (s *Server) consoleSettings(w http.ResponseWriter, r *http.Request) {
	s.renderSettings(w, r, "")
}

func (s *Server) renderSettings(w http.ResponseWriter, r *http.Request, flash string) {
	bs, err := s.pg.Queries.GetBankSettings(r.Context())
	if err != nil {
		s.log.Error("get settings", "err", err)
		http.Error(w, "settings error", http.StatusInternalServerError)
		return
	}
	canEdit := false
	if su, ok := userFromContext(r.Context()); ok {
		canEdit = canManageSettings(su.Role)
	}
	s.html(w)
	_ = template.SettingsPanel(bs, canEdit, flash).Render(r.Context(), w)
}

func (s *Server) consoleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireRole(w, r, canManageSettings)
	if !ok {
		return
	}
	_ = r.ParseForm()
	threshold, terr := money.ParseEuros(r.PostFormValue("maker_checker_threshold"))
	limit, lerr := money.ParseEuros(r.PostFormValue("default_transfer_limit"))
	if terr != nil || lerr != nil || threshold < 0 || limit < 0 {
		s.renderSettings(w, r, "Enter valid, non-negative amounts.")
		return
	}
	if err := s.pg.Queries.UpdateBankSettings(r.Context(), sqlc.UpdateBankSettingsParams{
		ThresholdMinor: threshold, DefaultLimitMinor: limit, Actor: actor.UserID,
	}); err != nil {
		s.renderSettings(w, r, "Could not save: "+dbErrorMessage(err))
		return
	}
	refresh(w) // lists that show limits/thresholds re-pull
	s.audit(r.Context(), actor, "update_settings", nil, map[string]any{
		"maker_checker_threshold_minor": threshold, "default_transfer_limit_minor": limit,
	})
	s.renderSettings(w, r, "Settings saved.")
}
