package api

import (
	"net/http"
	"strconv"

	sqlc "github.com/minhtt159/bank0/internal/db/sqlc"
	template "github.com/minhtt159/bank0/web/template"
)

// Operator-console fraud policy (Rec 22/25): the warning-rules engine and the AML
// watchlist. Both mirror the Settings panel's RBAC — all staff may view, only admins
// (canManageSettings) may mutate — and every change is written to admin_actions via
// s.audit, exactly like update_settings.

// --- validation sets (mirror the DB CHECK constraints so the flash is friendly) --

var (
	warnCategories = map[string]bool{
		"cop_no_match": true, "cop_close_match": true, "cop_unable": true,
		"guided_steer": true, "high_value": true, "risk_warning": true, "other": true,
	}
	warnSeverities = map[string]bool{"info": true, "warning": true, "critical": true}
	warnDecisions  = map[string]bool{"warn": true, "review": true, "block": true}
	warnBands      = map[string]bool{"low": true, "medium": true, "high": true}
)

// ---- warning rules ------------------------------------------------------

func (s *Server) consoleWarningRules(w http.ResponseWriter, r *http.Request) {
	s.html(w)
	_ = template.WarningRulesPanel().Render(r.Context(), w)
}

func (s *Server) renderWarningRules(w http.ResponseWriter, r *http.Request, flash string) {
	rules, err := s.pg.Queries.ListWarningRules(r.Context())
	if err != nil {
		s.log.Error("list warning rules", "err", err)
		http.Error(w, "warning rules error", http.StatusInternalServerError)
		return
	}
	canEdit := false
	if su, ok := userFromContext(r.Context()); ok {
		canEdit = canManageSettings(su.Role)
	}
	s.html(w)
	_ = template.WarningRulesList(rules, canEdit, flash).Render(r.Context(), w)
}

func (s *Server) consoleWarningRulesResults(w http.ResponseWriter, r *http.Request) {
	s.renderWarningRules(w, r, "")
}

// parseWarningRule reads and validates the create/edit form. It returns the parsed
// params (id-less) and "" on success, or a zero value and a flash message on error.
func parseWarningRule(r *http.Request) (sqlc.CreateWarningRuleParams, string) {
	_ = r.ParseForm()
	reason := strOrNil(r.PostFormValue("match_reason_code"))
	band := strOrNil(r.PostFormValue("match_min_band"))
	if reason == nil && band == nil {
		return sqlc.CreateWarningRuleParams{}, "Set at least one match key (reason code or minimum band)."
	}
	if band != nil && !warnBands[*band] {
		return sqlc.CreateWarningRuleParams{}, "Minimum band must be low, medium or high."
	}
	category := r.PostFormValue("category")
	if !warnCategories[category] {
		return sqlc.CreateWarningRuleParams{}, "Choose a valid warning category."
	}
	severity := r.PostFormValue("severity")
	if !warnSeverities[severity] {
		return sqlc.CreateWarningRuleParams{}, "Severity must be info, warning or critical."
	}
	decision := r.PostFormValue("decision")
	if !warnDecisions[decision] {
		return sqlc.CreateWarningRuleParams{}, "Decision must be warn, review or block."
	}
	cooling, cerr := strconv.Atoi(r.PostFormValue("cooling_off_seconds"))
	if cerr != nil || cooling < 0 || cooling > 86400 {
		return sqlc.CreateWarningRuleParams{}, "Cooling-off must be a whole number of seconds between 0 and 86400."
	}
	priority, perr := strconv.Atoi(r.PostFormValue("priority"))
	if perr != nil {
		return sqlc.CreateWarningRuleParams{}, "Priority must be a whole number."
	}
	return sqlc.CreateWarningRuleParams{
		MatchReasonCode:   reason,
		MatchMinBand:      band,
		Category:          category,
		Headline:          r.PostFormValue("headline"),
		Body:              r.PostFormValue("body"),
		Severity:          severity,
		Decision:          decision,
		RequiredAck:       r.PostFormValue("required_ack") == "true",
		CoolingOffSeconds: int32(cooling),
		Priority:          int32(priority),
		Active:            r.PostFormValue("active") == "true",
	}, ""
}

func (s *Server) consoleCreateWarningRule(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireRole(w, r, canManageSettings)
	if !ok {
		return
	}
	p, flash := parseWarningRule(r)
	if flash != "" {
		s.renderWarningRules(w, r, flash)
		return
	}
	id, err := s.pg.Queries.CreateWarningRule(r.Context(), p)
	if err != nil {
		s.renderWarningRules(w, r, "Could not create rule: "+s.dbFlash(r, err))
		return
	}
	refresh(w)
	s.audit(r.Context(), actor, "create_warning_rule", &id, map[string]any{
		"match_reason_code": p.MatchReasonCode, "match_min_band": p.MatchMinBand,
		"category": p.Category, "decision": p.Decision, "severity": p.Severity,
	})
	s.renderWarningRules(w, r, "Warning rule created.")
}

func (s *Server) consoleUpdateWarningRule(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireRole(w, r, canManageSettings)
	if !ok {
		return
	}
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid rule id")
		return
	}
	p, flash := parseWarningRule(r)
	if flash != "" {
		s.renderWarningRules(w, r, flash)
		return
	}
	if err := s.pg.Queries.UpdateWarningRule(r.Context(), sqlc.UpdateWarningRuleParams{
		MatchReasonCode:   p.MatchReasonCode,
		MatchMinBand:      p.MatchMinBand,
		Category:          p.Category,
		Headline:          p.Headline,
		Body:              p.Body,
		Severity:          p.Severity,
		Decision:          p.Decision,
		RequiredAck:       p.RequiredAck,
		CoolingOffSeconds: p.CoolingOffSeconds,
		Priority:          p.Priority,
		Active:            p.Active,
		ID:                id,
	}); err != nil {
		s.renderWarningRules(w, r, "Could not update rule: "+s.dbFlash(r, err))
		return
	}
	refresh(w)
	s.audit(r.Context(), actor, "update_warning_rule", &id, map[string]any{
		"category": p.Category, "decision": p.Decision, "severity": p.Severity, "active": p.Active,
	})
	s.renderWarningRules(w, r, "Warning rule updated.")
}

func (s *Server) consoleToggleWarningRule(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireRole(w, r, canManageSettings)
	if !ok {
		return
	}
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid rule id")
		return
	}
	active := r.PostFormValue("active") == "true"
	if err := s.pg.Queries.SetWarningRuleActive(r.Context(), sqlc.SetWarningRuleActiveParams{Active: active, ID: id}); err != nil {
		s.renderWarningRules(w, r, "Could not update rule: "+s.dbFlash(r, err))
		return
	}
	refresh(w)
	s.audit(r.Context(), actor, "toggle_warning_rule", &id, map[string]any{"active": active})
	if active {
		s.renderWarningRules(w, r, "Warning rule activated.")
	} else {
		s.renderWarningRules(w, r, "Warning rule deactivated.")
	}
}

// ---- watchlist ----------------------------------------------------------

func (s *Server) consoleWatchlist(w http.ResponseWriter, r *http.Request) {
	s.html(w)
	_ = template.WatchlistPanel().Render(r.Context(), w)
}

func (s *Server) renderWatchlist(w http.ResponseWriter, r *http.Request, flash string) {
	entries, err := s.pg.Queries.ListWatchlistEntries(r.Context())
	if err != nil {
		s.log.Error("list watchlist", "err", err)
		http.Error(w, "watchlist error", http.StatusInternalServerError)
		return
	}
	canEdit := false
	if su, ok := userFromContext(r.Context()); ok {
		canEdit = canManageSettings(su.Role)
	}
	s.html(w)
	_ = template.WatchlistList(entries, canEdit, flash).Render(r.Context(), w)
}

func (s *Server) consoleWatchlistResults(w http.ResponseWriter, r *http.Request) {
	s.renderWatchlist(w, r, "")
}

func (s *Server) consoleCreateWatchlistEntry(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireRole(w, r, canManageSettings)
	if !ok {
		return
	}
	_ = r.ParseForm()
	pattern := r.PostFormValue("pattern")
	if strOrNil(pattern) == nil {
		s.renderWatchlist(w, r, "Enter a non-empty ILIKE pattern (use % wildcards).")
		return
	}
	id, err := s.pg.Queries.CreateWatchlistEntry(r.Context(), sqlc.CreateWatchlistEntryParams{
		Pattern: pattern, Reason: r.PostFormValue("reason"), Active: true,
	})
	if err != nil {
		s.renderWatchlist(w, r, "Could not add entry: "+s.dbFlash(r, err))
		return
	}
	refresh(w)
	s.audit(r.Context(), actor, "create_watchlist_entry", &id, map[string]any{
		"pattern": pattern, "reason": r.PostFormValue("reason"),
	})
	s.renderWatchlist(w, r, "Watchlist entry added.")
}

func (s *Server) consoleToggleWatchlistEntry(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireRole(w, r, canManageSettings)
	if !ok {
		return
	}
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid entry id")
		return
	}
	active := r.PostFormValue("active") == "true"
	if err := s.pg.Queries.SetWatchlistEntryActive(r.Context(), sqlc.SetWatchlistEntryActiveParams{Active: active, ID: id}); err != nil {
		s.renderWatchlist(w, r, "Could not update entry: "+s.dbFlash(r, err))
		return
	}
	refresh(w)
	s.audit(r.Context(), actor, "toggle_watchlist_entry", &id, map[string]any{"active": active})
	if active {
		s.renderWatchlist(w, r, "Watchlist entry reactivated.")
	} else {
		s.renderWatchlist(w, r, "Watchlist entry deactivated.")
	}
}
