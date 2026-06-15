package api

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/mux"

	"github.com/minhtt159/bank0/internal/db"
	sqlc "github.com/minhtt159/bank0/internal/db/sqlc"
	"github.com/minhtt159/bank0/internal/iban"
	"github.com/minhtt159/bank0/internal/money"
	template "github.com/minhtt159/bank0/web/template"
)

// ---- role helpers -------------------------------------------------------

func canActOnMoney(role string) bool {
	return role == string(sqlc.UserRoleOperator) || role == string(sqlc.UserRoleAdmin)
}

func canManageUsers(role string) bool { return role == string(sqlc.UserRoleAdmin) }

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

func (s *Server) consoleUsers(w http.ResponseWriter, r *http.Request) {
	canManage := false
	if su, ok := userFromContext(r.Context()); ok {
		canManage = canManageUsers(su.Role)
	}
	s.html(w)
	_ = template.UsersPanel(canManage).Render(r.Context(), w)
}

func (s *Server) consoleUsersResults(w http.ResponseWriter, r *http.Request) {
	q := searchQ(r)
	ts, cid := pageCursor(r)
	limit := s.consolePageLimit(r)
	rows, err := s.pg.Queries.SearchUsers(r.Context(), sqlc.SearchUsersParams{
		Q: q, Cursor: ts, CursorID: cid, PageLimit: limit + 1,
	})
	if err != nil {
		s.log.Error("search users", "err", err)
		http.Error(w, "users error", http.StatusInternalServerError)
		return
	}
	rows, lastTs, lastID, hasMore := paginate(rows, limit, func(u sqlc.SearchUsersRow) (time.Time, uuid.UUID) {
		return u.CreatedAt, u.ID
	})
	prev, next := pagerLinks(r, "/console/users/results", q, lastTs, lastID, hasMore)
	s.html(w)
	_ = template.UsersRows(rows, prev, next).Render(r.Context(), w)
}

func (s *Server) consoleAccounts(w http.ResponseWriter, r *http.Request) {
	s.html(w)
	_ = template.AccountsPanel().Render(r.Context(), w)
}

func (s *Server) consoleAccountsResults(w http.ResponseWriter, r *http.Request) {
	q := searchQ(r)
	ts, cid := pageCursor(r)
	limit := s.consolePageLimit(r)
	rows, err := s.pg.Queries.SearchAccounts(r.Context(), sqlc.SearchAccountsParams{
		Q: q, Cursor: ts, CursorID: cid, PageLimit: limit + 1,
	})
	if err != nil {
		s.log.Error("search accounts", "err", err)
		http.Error(w, "accounts error", http.StatusInternalServerError)
		return
	}
	rows, lastTs, lastID, hasMore := paginate(rows, limit, func(a sqlc.SearchAccountsRow) (time.Time, uuid.UUID) {
		return a.CreatedAt, a.ID
	})
	prev, next := pagerLinks(r, "/console/accounts/results", q, lastTs, lastID, hasMore)
	s.html(w)
	_ = template.AccountsRows(rows, prev, next).Render(r.Context(), w)
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

// ---- users (rail) -------------------------------------------------------

func (s *Server) consoleNewUserForm(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireRole(w, r, canManageUsers); !ok {
		return
	}
	s.html(w)
	_ = template.CreateUserForm("").Render(r.Context(), w)
}

func (s *Server) consoleCreateUser(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireRole(w, r, canManageUsers)
	if !ok {
		return
	}
	_ = r.ParseForm()
	role := strings.TrimSpace(r.PostFormValue("role"))
	if role == "" {
		role = string(sqlc.UserRoleCustomer)
	}
	if !validRole(role) {
		s.html(w)
		_ = template.CreateUserForm("Invalid role.").Render(r.Context(), w)
		return
	}
	id, err := s.pg.Queries.CreateUser(r.Context(), sqlc.CreateUserParams{
		Username:    strings.TrimSpace(r.PostFormValue("username")),
		Password:    r.PostFormValue("password"),
		FullName:    strings.TrimSpace(r.PostFormValue("full_name")),
		Email:       strOrNil(r.PostFormValue("email")),
		PhoneNumber: strOrNil(r.PostFormValue("phone_number")),
		Role:        sqlc.UserRole(role),
	})
	if err != nil {
		s.html(w)
		_ = template.CreateUserForm("Could not create user: "+s.dbFlash(r, err)).Render(r.Context(), w)
		return
	}
	refresh(w)
	s.audit(r.Context(), actor, "create_user", &id, map[string]any{
		"username": strings.TrimSpace(r.PostFormValue("username")), "role": role,
	})
	s.renderUserDetail(w, r, id, "User created.")
}

func (s *Server) consoleUserDetail(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid user id")
		return
	}
	s.renderUserDetail(w, r, id, "")
}

func (s *Server) renderUserDetail(w http.ResponseWriter, r *http.Request, id uuid.UUID, flash string) {
	ctx := r.Context()
	u, err := s.pg.Queries.GetUserByID(ctx, id)
	if err != nil {
		s.mapDBError(w, r, err)
		return
	}
	accts, err := s.pg.Queries.ListAccountsByUser(ctx, id)
	if err != nil {
		s.mapDBError(w, r, err)
		return
	}
	role := ""
	if su, ok := userFromContext(ctx); ok {
		role = su.Role
	}
	s.html(w)
	_ = template.UserDetail(u, accts, canManageUsers(role), canActOnMoney(role), flash).Render(ctx, w)
}

func (s *Server) consoleUpdateUser(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireRole(w, r, canManageUsers)
	if !ok {
		return
	}
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid user id")
		return
	}
	_ = r.ParseForm()
	var status sqlc.NullUserStatus
	if v := strings.TrimSpace(r.PostFormValue("status")); v != "" {
		status = sqlc.NullUserStatus{UserStatus: sqlc.UserStatus(v), Valid: true}
	}
	err = s.pg.Queries.UpdateUserInfo(r.Context(), sqlc.UpdateUserInfoParams{
		UserID:      id,
		FullName:    strOrNil(r.PostFormValue("full_name")),
		Email:       strOrNil(r.PostFormValue("email")),
		PhoneNumber: strOrNil(r.PostFormValue("phone_number")),
		Status:      status,
	})
	if err != nil {
		s.renderUserDetail(w, r, id, "Could not save: "+s.dbFlash(r, err))
		return
	}
	refresh(w)
	s.audit(r.Context(), actor, "update_user", &id, map[string]any{"status": r.PostFormValue("status")})
	s.renderUserDetail(w, r, id, "Details saved.")
}

// consoleRevokeSessions force-revokes every active refresh token for a user
// (docs/06 "log out everywhere" / operator force-revoke). Admin only.
func (s *Server) consoleRevokeSessions(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireRole(w, r, canManageUsers)
	if !ok {
		return
	}
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid user id")
		return
	}
	n, err := s.pg.RevokeUserRefresh(r.Context(), id)
	if err != nil {
		s.renderUserDetail(w, r, id, "Could not revoke sessions: "+s.dbFlash(r, err))
		return
	}
	refresh(w)
	s.audit(r.Context(), actor, "revoke_sessions", &id, map[string]any{"revoked": n})
	s.renderUserDetail(w, r, id, fmt.Sprintf("Revoked %d active app session(s).", n))
}

// ---- accounts (rail actions; re-render the owner's detail) --------------

func (s *Server) consoleCreateAccount(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireRole(w, r, canActOnMoney)
	if !ok {
		return
	}
	userID, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid user id")
		return
	}
	_ = r.ParseForm()
	var limit int64 // 0 -> create_account uses the configured default (bank_settings)
	if v := strings.TrimSpace(r.PostFormValue("limit")); v != "" {
		if m, perr := money.ParseEuros(v); perr == nil {
			limit = m
		}
	}
	normIban := iban.Normalize(r.PostFormValue("iban"))
	if !iban.IsValid(normIban) {
		s.renderUserDetail(w, r, userID, "Invalid IBAN: failed checksum/format validation.")
		return
	}
	acctID, err := s.pg.Queries.CreateAccount(r.Context(), sqlc.CreateAccountParams{
		UserID:             userID,
		Iban:               normIban,
		Pin:                strings.TrimSpace(r.PostFormValue("pin")),
		TransferLimitMinor: limit,
	})
	if err != nil {
		s.renderUserDetail(w, r, userID, "Could not create account: "+s.dbFlash(r, err))
		return
	}
	refresh(w)
	s.audit(r.Context(), actor, "create_account", &acctID, map[string]any{
		"iban": normIban, "user_id": userID.String(),
	})
	s.renderUserDetail(w, r, userID, "Account created.")
}

// accountOwnerOrFail resolves the owning user of an account for re-rendering.
func (s *Server) accountOwnerOrFail(w http.ResponseWriter, r *http.Request, acctID uuid.UUID) (uuid.UUID, bool) {
	owner, err := s.pg.Queries.AccountOwner(r.Context(), acctID)
	if err != nil {
		s.mapDBError(w, r, err)
		return uuid.Nil, false
	}
	if owner == nil {
		writeError(w, http.StatusBadRequest, "bad_request", "account has no owner")
		return uuid.Nil, false
	}
	return *owner, true
}

// consoleAccountContext is the shared preamble for the account rail actions:
// money-role gate -> {id} parse -> resolve the owning user (for the re-render). It
// writes the response and returns ok=false on any problem.
func (s *Server) consoleAccountContext(w http.ResponseWriter, r *http.Request) (actor db.SessionUser, acctID, owner uuid.UUID, ok bool) {
	if actor, ok = s.requireRole(w, r, canActOnMoney); !ok {
		return
	}
	var err error
	if acctID, err = pathID(r); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid account id")
		return db.SessionUser{}, uuid.Nil, uuid.Nil, false
	}
	owner, ok = s.accountOwnerOrFail(w, r, acctID)
	return
}

// consoleMoneyDir bundles the pieces that distinguish a console credit from a
// withdrawal, so consoleMoveMoney can drive both. post is the below-threshold call
// that posts immediately; transferKind/requestDesc drive the above-threshold path,
// which stages a PENDING transfer + enqueues the approval row in ONE atomic DB
// call (request_money_with_approval) — see MAKER-CHECKER-ATOMICITY.
type consoleMoneyDir struct {
	kind           string            // "credit" / "withdraw" (also the approval-detail tag + error verb)
	noun           string            // "Credit" / "Withdrawal" (threshold message)
	postedVerb     string            // "Credited" / "Withdrew" (success message)
	auditPosted    string            // audit action when posted directly
	auditRequested string            // audit action when routed to approvals
	transferKind   sqlc.TransferKind // 'deposit' / 'withdrawal' for the staged maker-checker transfer
	requestDesc    string            // description on the staged (awaiting-approval) transfer
	post           func(key string, acct uuid.UUID, amount int64) (uuid.UUID, error)
}

// consoleMoveMoney is the shared body for consoleCredit/consoleWithdraw: gate +
// resolve owner, parse the amount + idempotency key, then either post directly or
// (above the maker-checker threshold) stage a PENDING request and enqueue it for a
// second admin. Only the per-direction DB calls and user-facing strings vary.
//
// Above the threshold we make a SINGLE DB call: request_money_with_approval stages
// the PENDING transfer + hold AND inserts the 4-eyes approval row in one
// transaction (rule 1). The previous two-call sequence (request_* then
// CreateApprovalRequest) could orphan a hold with no queue row if the second call
// failed mid-way (MAKER-CHECKER-ATOMICITY).
func (s *Server) consoleMoveMoney(w http.ResponseWriter, r *http.Request, dir consoleMoneyDir) {
	actor, acctID, owner, ok := s.consoleAccountContext(w, r)
	if !ok {
		return
	}
	_ = r.ParseForm()
	amount, perr := money.ParseEuros(r.PostFormValue("amount"))
	if perr != nil || amount <= 0 {
		s.renderUserDetail(w, r, owner, "Enter a positive amount.")
		return
	}
	key := strings.TrimSpace(r.PostFormValue("idempotency_key"))
	if key == "" {
		key = uuid.NewString()
	}
	ctx := r.Context()
	ra, err := s.pg.RequiresApproval(ctx, amount)
	if err != nil {
		s.renderUserDetail(w, r, owner, "Could not read policy: "+s.dbFlash(r, err))
		return
	}
	if ra.Required {
		detail, _ := json.Marshal(map[string]any{"amount_minor": amount, "kind": dir.kind, "account_id": acctID.String()})
		mc, err := s.pg.RequestMoneyWithApproval(ctx, actor.UserID, key, acctID, amount, dir.transferKind, dir.requestDesc, detail)
		if err != nil {
			s.renderUserDetail(w, r, owner, "Could not submit for approval: "+s.dbFlash(r, err))
			return
		}
		refresh(w)
		s.audit(ctx, actor, dir.auditRequested, &acctID, map[string]any{"amount_minor": amount, "transfer_id": mc.TransferID.String()})
		s.renderUserDetail(w, r, owner, dir.noun+" of "+money.FormatMinor(amount)+" exceeds the "+
			money.FormatMinor(ra.ThresholdMinor)+" threshold — sent to Approvals for a second admin.")
		return
	}
	tid, err := dir.post(key, acctID, amount)
	if err != nil {
		s.renderUserDetail(w, r, owner, "Could not "+dir.kind+": "+s.dbFlash(r, err))
		return
	}
	refresh(w)
	s.audit(ctx, actor, dir.auditPosted, &acctID, map[string]any{"amount_minor": amount, "transfer_id": tid.String()})
	s.renderUserDetail(w, r, owner, dir.postedVerb+" "+money.FormatMinor(amount)+".")
}

func (s *Server) consoleCredit(w http.ResponseWriter, r *http.Request) {
	s.consoleMoveMoney(w, r, consoleMoneyDir{
		kind: "credit", noun: "Credit", postedVerb: "Credited",
		auditPosted: "credit", auditRequested: "credit_requested",
		transferKind: sqlc.TransferKindDeposit, requestDesc: "Console credit (awaiting approval)",
		post: func(key string, acct uuid.UUID, amount int64) (uuid.UUID, error) {
			return s.pg.Queries.Deposit(r.Context(), sqlc.DepositParams{
				IdempotencyKey: key, AccountID: acct, AmountMinor: amount, Description: "Console credit",
			})
		},
	})
}

func (s *Server) consoleWithdraw(w http.ResponseWriter, r *http.Request) {
	s.consoleMoveMoney(w, r, consoleMoneyDir{
		kind: "withdraw", noun: "Withdrawal", postedVerb: "Withdrew",
		auditPosted: "withdraw", auditRequested: "withdraw_requested",
		transferKind: sqlc.TransferKindWithdrawal, requestDesc: "Console withdrawal (awaiting approval)",
		post: func(key string, acct uuid.UUID, amount int64) (uuid.UUID, error) {
			return s.pg.Queries.Withdraw(r.Context(), sqlc.WithdrawParams{
				IdempotencyKey: key, AccountID: acct, AmountMinor: amount, Description: "Console withdrawal",
			})
		},
	})
}

func (s *Server) consoleAccountStatus(w http.ResponseWriter, r *http.Request) {
	actor, acctID, owner, ok := s.consoleAccountContext(w, r)
	if !ok {
		return
	}
	_ = r.ParseForm()
	st := strings.TrimSpace(r.PostFormValue("status"))
	switch sqlc.AccountStatus(st) {
	case sqlc.AccountStatusActive, sqlc.AccountStatusFrozen, sqlc.AccountStatusClosed:
	default:
		s.renderUserDetail(w, r, owner, "Invalid status.")
		return
	}
	if err := s.pg.Queries.SetAccountStatus(r.Context(), sqlc.SetAccountStatusParams{
		AccountID: acctID, Status: sqlc.AccountStatus(st),
	}); err != nil {
		s.renderUserDetail(w, r, owner, "Could not change status: "+s.dbFlash(r, err))
		return
	}
	refresh(w)
	s.audit(r.Context(), actor, "set_account_status", &acctID, map[string]any{"status": st})
	s.renderUserDetail(w, r, owner, "Account "+st+".")
}

func (s *Server) consoleAccountLimit(w http.ResponseWriter, r *http.Request) {
	actor, acctID, owner, ok := s.consoleAccountContext(w, r)
	if !ok {
		return
	}
	_ = r.ParseForm()
	limit, perr := money.ParseEuros(r.PostFormValue("limit"))
	if perr != nil || limit < 0 {
		s.renderUserDetail(w, r, owner, "Enter a valid limit.")
		return
	}
	if err := s.pg.Queries.UpdateTransferLimit(r.Context(), sqlc.UpdateTransferLimitParams{
		AccountID: acctID, TransferLimitMinor: limit,
	}); err != nil {
		s.renderUserDetail(w, r, owner, "Could not set limit: "+s.dbFlash(r, err))
		return
	}
	refresh(w)
	s.audit(r.Context(), actor, "set_transfer_limit", &acctID, map[string]any{"limit_minor": limit})
	s.renderUserDetail(w, r, owner, "Transfer limit set to "+money.FormatMinor(limit)+".")
}

func (s *Server) consoleAccountDefault(w http.ResponseWriter, r *http.Request) {
	actor, acctID, owner, ok := s.consoleAccountContext(w, r)
	if !ok {
		return
	}
	if err := s.pg.Queries.SetDefaultAccount(r.Context(), sqlc.SetDefaultAccountParams{
		UserID: owner, AccountID: acctID,
	}); err != nil {
		s.renderUserDetail(w, r, owner, "Could not set default: "+s.dbFlash(r, err))
		return
	}
	refresh(w)
	s.audit(r.Context(), actor, "set_default_account", &acctID, nil)
	s.renderUserDetail(w, r, owner, "Default account updated.")
}

// ---- pending-queue actions ---------------------------------------------

// consoleTransfers renders the Transfers panel (search box + results container).
func (s *Server) consoleTransfers(w http.ResponseWriter, r *http.Request) {
	s.html(w)
	_ = template.TransfersPanel().Render(r.Context(), w)
}

// consoleTransfersResults shows ALL transfers (history), newest first, filtered
// by ?q and paginated by ?cursor. Pending rows are actionable for staff.
func (s *Server) consoleTransfersResults(w http.ResponseWriter, r *http.Request) {
	rows, prev, next, canAct, err := s.transfersPage(r)
	if err != nil {
		http.Error(w, "transfers error", http.StatusInternalServerError)
		return
	}
	s.html(w)
	_ = template.TransferTable(rows, canAct, prev, next, "").Render(r.Context(), w)
}

// renderTransfers re-renders the full table (used after post/cancel; no cursor).
func (s *Server) renderTransfers(w http.ResponseWriter, r *http.Request, flash string) {
	rows, prev, next, canAct, err := s.transfersPage(r)
	if err != nil {
		http.Error(w, "transfers error", http.StatusInternalServerError)
		return
	}
	s.html(w)
	_ = template.TransferTable(rows, canAct, prev, next, flash).Render(r.Context(), w)
}

func (s *Server) transfersPage(r *http.Request) ([]sqlc.SearchTransfersRow, string, string, bool, error) {
	ctx := r.Context()
	q := searchQ(r)
	ts, cid := pageCursor(r)
	limit := s.consolePageLimit(r)
	rows, err := s.pg.Queries.SearchTransfers(ctx, sqlc.SearchTransfersParams{
		Q: q, Cursor: ts, CursorID: cid, PageLimit: limit + 1,
	})
	if err != nil {
		s.log.Error("list transfers", "err", err)
		return nil, "", "", false, err
	}
	rows, lastTs, lastID, hasMore := paginate(rows, limit, func(t sqlc.SearchTransfersRow) (time.Time, uuid.UUID) {
		return t.RequestedAt, t.ID
	})
	prev, next := pagerLinks(r, "/console/transfers/results", q, lastTs, lastID, hasMore)
	canAct := false
	if su, ok := userFromContext(ctx); ok {
		canAct = canActOnMoney(su.Role)
	}
	return rows, prev, next, canAct, nil
}

func (s *Server) consolePostTransfer(w http.ResponseWriter, r *http.Request) {
	su, id, ok := s.consoleActionContext(w, r)
	if !ok {
		return
	}
	if _, err := s.pg.Queries.PostTransfer(r.Context(), id); err != nil {
		s.renderTransfers(w, r, "Could not post: "+s.dbFlash(r, err))
		return
	}
	s.audit(r.Context(), su, "post_transfer", &id, nil)
	s.renderTransfers(w, r, "Transfer posted.")
}

func (s *Server) consoleCancelTransfer(w http.ResponseWriter, r *http.Request) {
	su, id, ok := s.consoleActionContext(w, r)
	if !ok {
		return
	}
	reason := "cancelled via console by " + su.Username
	if _, err := s.pg.Queries.CancelTransfer(r.Context(), sqlc.CancelTransferParams{ID: id, Reason: reason}); err != nil {
		s.renderTransfers(w, r, "Could not cancel: "+s.dbFlash(r, err))
		return
	}
	s.audit(r.Context(), su, "cancel_transfer", &id, map[string]any{"reason": reason})
	s.renderTransfers(w, r, "Transfer cancelled.")
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

// ---- statement + transfer detail (drill-down) ---------------------------

func (s *Server) consoleStatement(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid account id")
		return
	}
	ts, cid := pageCursor(r)
	limit := s.consolePageLimit(r)
	rows, err := s.pg.Queries.AccountStatement(ctx, sqlc.AccountStatementParams{
		AccountID: id, Cursor: ts, CursorID: cid, PageLimit: limit + 1,
	})
	if err != nil {
		s.mapDBError(w, r, err)
		return
	}
	rows, lastTs, lastID, hasMore := paginate(rows, limit, func(e sqlc.AccountStatementRow) (time.Time, uuid.UUID) {
		return e.PostedAt, e.ID
	})
	prev, next := pagerLinks(r, "/console/accounts/"+id.String()+"/statement", nil, lastTs, lastID, hasMore)
	s.html(w)
	if isPagerNav(r) {
		_ = template.StatementBody(rows, prev, next).Render(ctx, w)
		return
	}
	acct, err := s.pg.Queries.GetAccount(ctx, id)
	if err != nil {
		s.mapDBError(w, r, err)
		return
	}
	_ = template.StatementView(acct, rows, prev, next).Render(ctx, w)
}

func (s *Server) consoleTransferDetail(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid transfer id")
		return
	}
	s.renderTransferDetail(w, r, id, "")
}

func (s *Server) renderTransferDetail(w http.ResponseWriter, r *http.Request, id uuid.UUID, flash string) {
	ctx := r.Context()
	t, err := s.pg.Queries.GetTransferDetail(ctx, id)
	if err != nil {
		s.mapDBError(w, r, err)
		return
	}
	legs, err := s.pg.Queries.TransferLegs(ctx, id)
	if err != nil {
		s.mapDBError(w, r, err)
		return
	}
	holds, _ := s.pg.Queries.HoldForTransfer(ctx, id)
	role := ""
	if su, ok := userFromContext(ctx); ok {
		role = su.Role
	}
	canReverse := canApprove(role) && t.Status == sqlc.TransferStatusPosted && t.Kind != sqlc.TransferKindReversal
	s.html(w)
	_ = template.TransferDetail(t, legs, holds, canReverse, flash).Render(ctx, w)
}

func (s *Server) consoleReverse(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireRole(w, r, canApprove)
	if !ok {
		return
	}
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid transfer id")
		return
	}
	_ = r.ParseForm()
	key := strings.TrimSpace(r.PostFormValue("idempotency_key"))
	if key == "" {
		key = uuid.NewString()
	}
	reason := strings.TrimSpace(r.PostFormValue("reason"))
	revID, err := s.pg.Queries.ReverseTransfer(r.Context(), sqlc.ReverseTransferParams{
		ID: id, IdempotencyKey: key, Reason: reason,
	})
	if err != nil {
		s.renderTransferDetail(w, r, id, "Could not reverse: "+s.dbFlash(r, err))
		return
	}
	refresh(w)
	s.audit(r.Context(), actor, "reverse", &id, map[string]any{"reversal_id": revID.String(), "reason": reason})
	s.renderTransferDetail(w, r, id, "Reversed — inverse entries posted.")
}

// ---- approvals (maker-checker) ------------------------------------------

func (s *Server) consoleApprovals(w http.ResponseWriter, r *http.Request) {
	role := ""
	if su, ok := userFromContext(r.Context()); ok {
		role = su.Role
	}
	s.html(w)
	_ = template.ApprovalsPanel(canApprove(role)).Render(r.Context(), w)
}

func (s *Server) renderApprovals(w http.ResponseWriter, r *http.Request, flash string) {
	ctx := r.Context()
	ts, cid := pageCursor(r)
	limit := s.consolePageLimit(r)
	rows, err := s.pg.Queries.ListPendingApprovals(ctx, sqlc.ListPendingApprovalsParams{
		Cursor: ts, CursorID: cid, PageLimit: limit + 1,
	})
	if err != nil {
		s.log.Error("list approvals", "err", err)
		http.Error(w, "approvals error", http.StatusInternalServerError)
		return
	}
	rows, lastTs, lastID, hasMore := paginate(rows, limit, func(a sqlc.ListPendingApprovalsRow) (time.Time, uuid.UUID) {
		return a.CreatedAt, a.ID
	})
	prev, next := pagerLinks(r, "/console/approvals/results", nil, lastTs, lastID, hasMore)
	canAct := false
	if su, ok := userFromContext(ctx); ok {
		canAct = canApprove(su.Role)
	}
	s.html(w)
	_ = template.ApprovalRows(rows, canAct, prev, next, flash).Render(ctx, w)
}

func (s *Server) consoleApprovalsResults(w http.ResponseWriter, r *http.Request) {
	s.renderApprovals(w, r, "")
}

func (s *Server) consoleApprove(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireRole(w, r, canApprove)
	if !ok {
		return
	}
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request id")
		return
	}
	tid, err := s.pg.Queries.ApproveRequest(r.Context(), sqlc.ApproveRequestParams{RequestID: id, Approver: actor.UserID})
	if err != nil {
		s.renderApprovals(w, r, "Could not approve: "+s.dbFlash(r, err))
		return
	}
	refresh(w)
	s.audit(r.Context(), actor, "approve", &tid, map[string]any{"request_id": id.String()})
	s.renderApprovals(w, r, "Approved and posted.")
}

func (s *Server) consoleReject(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireRole(w, r, canApprove)
	if !ok {
		return
	}
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request id")
		return
	}
	tid, err := s.pg.Queries.RejectRequest(r.Context(), sqlc.RejectRequestParams{
		RequestID: id, Approver: actor.UserID, Reason: "rejected via console by " + actor.Username,
	})
	if err != nil {
		s.renderApprovals(w, r, "Could not reject: "+s.dbFlash(r, err))
		return
	}
	refresh(w)
	s.audit(r.Context(), actor, "reject", &tid, map[string]any{"request_id": id.String()})
	s.renderApprovals(w, r, "Request rejected.")
}

// consoleActionContext extracts the session user + {id} and enforces the money
// role. It writes the response and returns ok=false on any problem.
func (s *Server) consoleActionContext(w http.ResponseWriter, r *http.Request) (db.SessionUser, uuid.UUID, bool) {
	u, ok := s.requireRole(w, r, canActOnMoney)
	if !ok {
		return db.SessionUser{}, uuid.Nil, false
	}
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid transfer id")
		return db.SessionUser{}, uuid.Nil, false
	}
	return u, id, true
}

// ---- disputes (triage queue) --------------------------------------------

func (s *Server) consoleDisputes(w http.ResponseWriter, r *http.Request) {
	canAct := false
	if su, ok := userFromContext(r.Context()); ok {
		canAct = canActOnMoney(su.Role)
	}
	s.html(w)
	_ = template.DisputesPanel(canAct).Render(r.Context(), w)
}

func (s *Server) renderDisputes(w http.ResponseWriter, r *http.Request, flash string) {
	ctx := r.Context()
	ts, cid := pageCursor(r)
	limit := s.consolePageLimit(r)
	rows, err := s.pg.Queries.ListDisputesAdmin(ctx, sqlc.ListDisputesAdminParams{
		Cursor: ts, CursorID: cid, PageLimit: limit + 1, // status NULL => all
	})
	if err != nil {
		s.log.Error("list disputes", "err", err)
		http.Error(w, "disputes error", http.StatusInternalServerError)
		return
	}
	rows, lastTs, lastID, hasMore := paginate(rows, limit, func(d sqlc.ListDisputesAdminRow) (time.Time, uuid.UUID) {
		return d.CreatedAt, d.ID
	})
	prev, next := pagerLinks(r, "/console/disputes/results", nil, lastTs, lastID, hasMore)
	canAct := false
	if su, ok := userFromContext(ctx); ok {
		canAct = canActOnMoney(su.Role)
	}
	s.html(w)
	_ = template.DisputeRows(rows, canAct, prev, next, flash).Render(ctx, w)
}

func (s *Server) consoleDisputesResults(w http.ResponseWriter, r *http.Request) {
	s.renderDisputes(w, r, "")
}

// consoleResolveDispute drives resolve_dispute from the console. Gated to
// operators/admins (canActOnMoney) — matching the JSON admin handler; the DB
// function audits the transition in admin_actions. status comes from the query
// (?status=), the optional note from the form body.
func (s *Server) consoleResolveDispute(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireRole(w, r, canActOnMoney)
	if !ok {
		return
	}
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid dispute id")
		return
	}
	status := strings.TrimSpace(r.FormValue("status"))
	note := strings.TrimSpace(r.PostFormValue("resolution_note"))
	if !validResolveStatus(status) {
		s.renderDisputes(w, r, "Invalid status.")
		return
	}
	if _, err := s.pg.Queries.ResolveDispute(r.Context(), sqlc.ResolveDisputeParams{
		DisputeID: id, Resolver: actor.UserID, Status: sqlc.DisputeStatus(status), Note: note,
	}); err != nil {
		s.renderDisputes(w, r, "Could not resolve: "+s.dbFlash(r, err))
		return
	}
	refresh(w)
	s.renderDisputes(w, r, "Dispute "+status+".")
}
