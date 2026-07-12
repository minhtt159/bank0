package api

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/mux"

	"github.com/minhtt159/bank0/internal/db"
	sqlc "github.com/minhtt159/bank0/internal/db/sqlc"
	template "github.com/minhtt159/bank0/web/template"
)

// This file holds the operator-console core: role gates, the shared request
// helpers, the keyset-pagination machinery, and the top-level panels
// (home/dashboard/reconcile/audit). The per-domain handlers live alongside it in
// console_{users,accounts,transfers,approvals,disputes}.go.

// ---- role helpers -------------------------------------------------------

func canActOnMoney(role string) bool {
	return role == string(sqlc.UserRoleOperator) || role == string(sqlc.UserRoleAdmin)
}

func canManageUsers(role string) bool { return role == string(sqlc.UserRoleAdmin) }

// canCreateUsers gates registering users and editing per-user invitation quotas:
// operators and admins. (Editing existing user details/status stays admin-only via
// canManageUsers.)
func canCreateUsers(role string) bool {
	return role == string(sqlc.UserRoleOperator) || role == string(sqlc.UserRoleAdmin)
}

// canManageSettings gates editing bank policy (the Settings panel): admins only.
// All staff may view it read-only.
func canManageSettings(role string) bool { return role == string(sqlc.UserRoleAdmin) }

// canApprove gates the maker-checker queue: only admins approve/reject.
func canApprove(role string) bool { return role == string(sqlc.UserRoleAdmin) }

func (s *Server) requireRole(w http.ResponseWriter, r *http.Request, allow func(string) bool) (db.SessionUser, bool) {
	u, ok := userFromContext(r.Context())
	if !ok || !allow(u.Role) {
		writeError(w, http.StatusForbidden, "forbidden", "your role cannot perform this action")
		return db.SessionUser{}, false
	}
	return u, true
}

// ---- small helpers ------------------------------------------------------

func (s *Server) html(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
}

// refresh tells the main-panel lists to reload after a rail mutation.
func refresh(w http.ResponseWriter) { w.Header().Set("HX-Trigger", "bank0:refresh") }

func strOrNil(s string) *string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return &s
}

func pathID(r *http.Request) (uuid.UUID, error) { return uuid.Parse(mux.Vars(r)["id"]) }

// --- cursor pagination helpers (Prev / Next) -----------------------------
//
// Keyset (cursor) pagination is forward-only by nature, but Prev/Next needs to
// step backwards too. We stay stateless on the server by carrying the stack of
// cursors for the pages already visited in a ?hist param. "Next" pushes the
// current page's cursor; "Prev" pops the stack. No page numbers, no COUNT(*),
// and each page is still a single indexed keyset query.

// pageCursor reads the composite keyset cursor (?cursor timestamp + ?cid id).
// Both nil = first page. The id tiebreak makes pagination correct even when many
// rows share a timestamp (e.g. a burst of inserts in one transaction).
func pageCursor(r *http.Request) (*time.Time, *uuid.UUID) {
	var ts *time.Time
	var id *uuid.UUID
	if c := r.URL.Query().Get("cursor"); c != "" {
		if t, err := time.Parse(time.RFC3339Nano, c); err == nil {
			ts = &t
		}
	}
	if c := r.URL.Query().Get("cid"); c != "" {
		if u, err := uuid.Parse(c); err == nil {
			id = &u
		}
	}
	return ts, id
}

// isPagerNav reports whether the request came from a Prev/Next button (vs. the
// first render of a drill-down view). Drives whether a fragment or full view
// is returned for the statement screen.
func isPagerNav(r *http.Request) bool { return r.URL.Query().Get("nav") == "1" }

// currentCursorStr encodes the cursor that produced the page being rendered
// ("" for the first page) as "ts|cid".
func currentCursorStr(r *http.Request) string {
	c := r.URL.Query().Get("cursor")
	if c == "" {
		return ""
	}
	return c + "|" + r.URL.Query().Get("cid")
}

// pageHistory decodes the ?hist stack (cursors of the preceding pages).
func pageHistory(r *http.Request) []string {
	s := r.URL.Query().Get("hist")
	if s == "" {
		return nil
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil
	}
	var h []string
	if json.Unmarshal(b, &h) != nil {
		return nil
	}
	return h
}

func encodeHistory(h []string) string {
	if len(h) == 0 {
		return ""
	}
	b, _ := json.Marshal(h)
	return base64.RawURLEncoding.EncodeToString(b)
}

// buildPageURL assembles a results URL for a given cursor + history stack.
func buildPageURL(base, cursor string, hist []string, q *string) string {
	v := url.Values{}
	v.Set("nav", "1")
	if cursor != "" {
		parts := strings.SplitN(cursor, "|", 2)
		v.Set("cursor", parts[0])
		if len(parts) == 2 {
			v.Set("cid", parts[1])
		}
	}
	if h := encodeHistory(hist); h != "" {
		v.Set("hist", h)
	}
	if q != nil {
		v.Set("q", *q)
	}
	return base + "?" + v.Encode()
}

// pagerLinks builds the Prev/Next URLs for a keyset-paginated list. lastTs/lastID
// are the cursor of the last row on the current page; hasMore says whether a
// following page exists. An empty string means "no such direction".
func pagerLinks(r *http.Request, base string, q *string, lastTs time.Time, lastID uuid.UUID, hasMore bool) (prev, next string) {
	hist := pageHistory(r)
	if len(hist) > 0 {
		// Prev: pop the cursor of the immediately preceding page.
		prev = buildPageURL(base, hist[len(hist)-1], hist[:len(hist)-1], q)
	}
	if hasMore {
		// Next: push the current page's cursor onto the stack.
		newHist := append(append([]string{}, hist...), currentCursorStr(r))
		nextCursor := lastTs.Format(time.RFC3339Nano) + "|" + lastID.String()
		next = buildPageURL(base, nextCursor, newHist, q)
	}
	return prev, next
}

// paginate trims a keyset page fetched with PageLimit=limit+1: it drops the probe
// row, reports whether a following page exists, and returns the (timestamp, id)
// cursor of the last row on the page. cursorOf reads the keyset columns from a row.
// Callers build their own pager links from the returned cursor, since the base URL
// / q / nav handling differs per screen.
func paginate[T any](rows []T, limit int32, cursorOf func(T) (time.Time, uuid.UUID)) (page []T, lastTs time.Time, lastID uuid.UUID, hasMore bool) {
	hasMore = int32(len(rows)) > limit
	if hasMore {
		rows = rows[:limit]
	}
	if hasMore && len(rows) > 0 {
		lastTs, lastID = cursorOf(rows[len(rows)-1])
	}
	return rows, lastTs, lastID, hasMore
}

// ---- shell + main-panel screens ----------------------------------------

func (s *Server) consoleHome(w http.ResponseWriter, r *http.Request) {
	su, _ := userFromContext(r.Context())
	pending, _ := s.pg.Queries.CountPendingApprovals(r.Context())
	s.html(w)
	_ = template.Shell(su.Username, su.Role, int(pending)).Render(r.Context(), w)
}

func (s *Server) consoleDashboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	stats, err := s.pg.Queries.DashboardStats(ctx)
	if err != nil {
		s.log.Error("dashboard stats", "err", err)
		http.Error(w, "dashboard error", http.StatusInternalServerError)
		return
	}
	issues, err := s.pg.Reconcile(ctx)
	if err != nil {
		s.log.Error("reconcile", "err", err)
		http.Error(w, "reconcile error", http.StatusInternalServerError)
		return
	}
	s.html(w)
	_ = template.DashboardCards(stats, len(issues) == 0, issues, s.cfg.App.Version).Render(ctx, w)
}

// searchQ returns the optional ?q= as a *string (nil when empty) for the
// unified list+search queries.
func searchQ(r *http.Request) *string {
	return strOrNil(r.URL.Query().Get("q"))
}

func (s *Server) consoleReconcile(w http.ResponseWriter, r *http.Request) {
	issues, err := s.pg.Reconcile(r.Context())
	if err != nil {
		s.log.Error("reconcile", "err", err)
		http.Error(w, "reconcile error", http.StatusInternalServerError)
		return
	}
	s.html(w)
	_ = template.ReconcilePanel(issues).Render(r.Context(), w)
}

// ---- audit log ----------------------------------------------------------

func (s *Server) consoleAudit(w http.ResponseWriter, r *http.Request) {
	s.html(w)
	_ = template.AuditPanel().Render(r.Context(), w)
}

func (s *Server) consoleAuditResults(w http.ResponseWriter, r *http.Request) {
	q := searchQ(r)
	ts, cid := pageCursor(r)
	limit := s.consolePageLimit(r)
	rows, err := s.pg.Queries.ListAuditLog(r.Context(), sqlc.ListAuditLogParams{
		Q: q, Cursor: ts, CursorID: cid, PageLimit: limit + 1,
	})
	if err != nil {
		s.log.Error("list audit", "err", err)
		http.Error(w, "audit error", http.StatusInternalServerError)
		return
	}
	rows, lastTs, lastID, hasMore := paginate(rows, limit, func(a sqlc.ListAuditLogRow) (time.Time, uuid.UUID) {
		return a.CreatedAt, a.ID
	})
	prev, next := pagerLinks(r, "/console/audit/results", q, lastTs, lastID, hasMore)
	s.html(w)
	_ = template.AuditRows(rows, prev, next).Render(r.Context(), w)
}
