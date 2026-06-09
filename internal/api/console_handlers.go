package api

import (
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
	"github.com/minhtt159/bank0/internal/money"
	template "github.com/minhtt159/bank0/web/template"
)

// ---- role helpers -------------------------------------------------------

func canActOnMoney(role string) bool {
	return role == string(sqlc.UserRoleOperator) || role == string(sqlc.UserRoleAdmin)
}

func canManageUsers(role string) bool { return role == string(sqlc.UserRoleAdmin) }

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

func (s *Server) html(w http.ResponseWriter) { w.Header().Set("Content-Type", "text/html; charset=utf-8") }

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

// --- cursor pagination helpers (Load more) -------------------------------

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

func hasCursor(r *http.Request) bool { return r.URL.Query().Get("cursor") != "" }

// nextPageURL builds the "Load more" URL carrying the next keyset cursor (+ optional q).
func nextPageURL(base string, ts time.Time, id uuid.UUID, q *string) string {
	v := url.Values{}
	v.Set("cursor", ts.Format(time.RFC3339Nano))
	v.Set("cid", id.String())
	if q != nil {
		v.Set("q", *q)
	}
	return base + "?" + v.Encode()
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
	rows, err := s.pg.Queries.SearchUsers(r.Context(), sqlc.SearchUsersParams{
		Q: searchQ(r), PageLimit: s.cfg.Server.DefaultPageLimit,
	})
	if err != nil {
		s.log.Error("search users", "err", err)
		http.Error(w, "users error", http.StatusInternalServerError)
		return
	}
	s.html(w)
	_ = template.UsersRows(rows).Render(r.Context(), w)
}

func (s *Server) consoleAccounts(w http.ResponseWriter, r *http.Request) {
	s.html(w)
	_ = template.AccountsPanel().Render(r.Context(), w)
}

func (s *Server) consoleAccountsResults(w http.ResponseWriter, r *http.Request) {
	rows, err := s.pg.Queries.SearchAccounts(r.Context(), sqlc.SearchAccountsParams{
		Q: searchQ(r), PageLimit: s.cfg.Server.DefaultPageLimit,
	})
	if err != nil {
		s.log.Error("search accounts", "err", err)
		http.Error(w, "accounts error", http.StatusInternalServerError)
		return
	}
	s.html(w)
	_ = template.AccountsRows(rows).Render(r.Context(), w)
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
		_ = template.CreateUserForm("Could not create user: " + dbErrorMessage(err)).Render(r.Context(), w)
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
		mapDBError(w, err)
		return
	}
	accts, err := s.pg.Queries.ListAccountsByUser(ctx, id)
	if err != nil {
		mapDBError(w, err)
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
		s.renderUserDetail(w, r, id, "Could not save: "+dbErrorMessage(err))
		return
	}
	refresh(w)
	s.audit(r.Context(), actor, "update_user", &id, map[string]any{"status": r.PostFormValue("status")})
	s.renderUserDetail(w, r, id, "Details saved.")
}

// consoleRevokeSessions force-revokes every active refresh token for a user
// (docs/07 "log out everywhere" / operator force-revoke). Admin only.
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
		s.renderUserDetail(w, r, id, "Could not revoke sessions: "+dbErrorMessage(err))
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
	limit := int64(50000)
	if v := strings.TrimSpace(r.PostFormValue("limit")); v != "" {
		if m, perr := money.ParseEuros(v); perr == nil {
			limit = m
		}
	}
	acctID, err := s.pg.Queries.CreateAccount(r.Context(), sqlc.CreateAccountParams{
		UserID:             userID,
		Iban:               strings.TrimSpace(r.PostFormValue("iban")),
		Pin:                strings.TrimSpace(r.PostFormValue("pin")),
		TransferLimitMinor: limit,
	})
	if err != nil {
		s.renderUserDetail(w, r, userID, "Could not create account: "+dbErrorMessage(err))
		return
	}
	refresh(w)
	s.audit(r.Context(), actor, "create_account", &acctID, map[string]any{
		"iban": strings.TrimSpace(r.PostFormValue("iban")), "user_id": userID.String(),
	})
	s.renderUserDetail(w, r, userID, "Account created.")
}

// accountOwnerOrFail resolves the owning user of an account for re-rendering.
func (s *Server) accountOwnerOrFail(w http.ResponseWriter, r *http.Request, acctID uuid.UUID) (uuid.UUID, bool) {
	owner, err := s.pg.Queries.AccountOwner(r.Context(), acctID)
	if err != nil {
		mapDBError(w, err)
		return uuid.Nil, false
	}
	if owner == nil {
		writeError(w, http.StatusBadRequest, "bad_request", "account has no owner")
		return uuid.Nil, false
	}
	return *owner, true
}

func (s *Server) consoleCredit(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireRole(w, r, canActOnMoney)
	if !ok {
		return
	}
	acctID, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid account id")
		return
	}
	owner, ok := s.accountOwnerOrFail(w, r, acctID)
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
	// Maker-checker: above the threshold, the credit becomes a PENDING deposit and
	// goes to the Approvals queue for a second admin instead of posting now.
	if amount > s.cfg.Admin.MakerCheckerThresholdMinor {
		tid, err := s.pg.Queries.RequestDeposit(ctx, sqlc.RequestDepositParams{
			IdempotencyKey: key, AccountID: acctID, AmountMinor: amount,
			Description: "Console credit (awaiting approval)",
		})
		if err != nil {
			s.renderUserDetail(w, r, owner, "Could not submit: "+dbErrorMessage(err))
			return
		}
		detail, _ := json.Marshal(map[string]any{"amount_minor": amount, "kind": "credit", "account_id": acctID.String()})
		if _, err := s.pg.Queries.CreateApprovalRequest(ctx, sqlc.CreateApprovalRequestParams{
			Maker: actor.UserID, TransferID: tid, Detail: detail,
		}); err != nil {
			s.renderUserDetail(w, r, owner, "Could not submit for approval: "+dbErrorMessage(err))
			return
		}
		refresh(w)
		s.audit(ctx, actor, "credit_requested", &acctID, map[string]any{"amount_minor": amount, "transfer_id": tid.String()})
		s.renderUserDetail(w, r, owner, "Credit of "+money.FormatMinor(amount)+" exceeds the "+
			money.FormatMinor(s.cfg.Admin.MakerCheckerThresholdMinor)+" threshold — sent to Approvals for a second admin.")
		return
	}
	tid, err := s.pg.Queries.Deposit(ctx, sqlc.DepositParams{
		IdempotencyKey: key,
		AccountID:      acctID,
		AmountMinor:    amount,
		Description:    "Console credit",
	})
	if err != nil {
		s.renderUserDetail(w, r, owner, "Could not credit: "+dbErrorMessage(err))
		return
	}
	refresh(w)
	s.audit(ctx, actor, "credit", &acctID, map[string]any{
		"amount_minor": amount, "transfer_id": tid.String(),
	})
	s.renderUserDetail(w, r, owner, "Credited "+money.FormatMinor(amount)+".")
}

func (s *Server) consoleWithdraw(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireRole(w, r, canActOnMoney)
	if !ok {
		return
	}
	acctID, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid account id")
		return
	}
	owner, ok := s.accountOwnerOrFail(w, r, acctID)
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
	// Maker-checker: above the threshold, the withdrawal is staged as PENDING
	// (the hold reserves funds) and routed to Approvals for a second admin.
	if amount > s.cfg.Admin.MakerCheckerThresholdMinor {
		tid, err := s.pg.Queries.RequestWithdrawal(ctx, sqlc.RequestWithdrawalParams{
			IdempotencyKey: key, AccountID: acctID, AmountMinor: amount,
			Description: "Console withdrawal (awaiting approval)",
		})
		if err != nil {
			s.renderUserDetail(w, r, owner, "Could not submit: "+dbErrorMessage(err))
			return
		}
		detail, _ := json.Marshal(map[string]any{"amount_minor": amount, "kind": "withdraw", "account_id": acctID.String()})
		if _, err := s.pg.Queries.CreateApprovalRequest(ctx, sqlc.CreateApprovalRequestParams{
			Maker: actor.UserID, TransferID: tid, Detail: detail,
		}); err != nil {
			s.renderUserDetail(w, r, owner, "Could not submit for approval: "+dbErrorMessage(err))
			return
		}
		refresh(w)
		s.audit(ctx, actor, "withdraw_requested", &acctID, map[string]any{"amount_minor": amount, "transfer_id": tid.String()})
		s.renderUserDetail(w, r, owner, "Withdrawal of "+money.FormatMinor(amount)+" exceeds the "+
			money.FormatMinor(s.cfg.Admin.MakerCheckerThresholdMinor)+" threshold — sent to Approvals for a second admin.")
		return
	}
	tid, err := s.pg.Queries.Withdraw(ctx, sqlc.WithdrawParams{
		IdempotencyKey: key,
		AccountID:      acctID,
		AmountMinor:    amount,
		Description:    "Console withdrawal",
	})
	if err != nil {
		s.renderUserDetail(w, r, owner, "Could not withdraw: "+dbErrorMessage(err))
		return
	}
	refresh(w)
	s.audit(ctx, actor, "withdraw", &acctID, map[string]any{
		"amount_minor": amount, "transfer_id": tid.String(),
	})
	s.renderUserDetail(w, r, owner, "Withdrew "+money.FormatMinor(amount)+".")
}

func (s *Server) consoleAccountStatus(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireRole(w, r, canActOnMoney)
	if !ok {
		return
	}
	acctID, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid account id")
		return
	}
	owner, ok := s.accountOwnerOrFail(w, r, acctID)
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
		s.renderUserDetail(w, r, owner, "Could not change status: "+dbErrorMessage(err))
		return
	}
	refresh(w)
	s.audit(r.Context(), actor, "set_account_status", &acctID, map[string]any{"status": st})
	s.renderUserDetail(w, r, owner, "Account "+st+".")
}

func (s *Server) consoleAccountLimit(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireRole(w, r, canActOnMoney)
	if !ok {
		return
	}
	acctID, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid account id")
		return
	}
	owner, ok := s.accountOwnerOrFail(w, r, acctID)
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
		s.renderUserDetail(w, r, owner, "Could not set limit: "+dbErrorMessage(err))
		return
	}
	refresh(w)
	s.audit(r.Context(), actor, "set_transfer_limit", &acctID, map[string]any{"limit_minor": limit})
	s.renderUserDetail(w, r, owner, "Transfer limit set to "+money.FormatMinor(limit)+".")
}

func (s *Server) consoleAccountDefault(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireRole(w, r, canActOnMoney)
	if !ok {
		return
	}
	acctID, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid account id")
		return
	}
	owner, ok := s.accountOwnerOrFail(w, r, acctID)
	if !ok {
		return
	}
	if err := s.pg.Queries.SetDefaultAccount(r.Context(), sqlc.SetDefaultAccountParams{
		UserID: owner, AccountID: acctID,
	}); err != nil {
		s.renderUserDetail(w, r, owner, "Could not set default: "+dbErrorMessage(err))
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
	rows, next, canAct, err := s.transfersPage(r)
	if err != nil {
		http.Error(w, "transfers error", http.StatusInternalServerError)
		return
	}
	s.html(w)
	if hasCursor(r) {
		_ = template.TransferItems(rows, canAct, next).Render(r.Context(), w)
	} else {
		_ = template.TransferTable(rows, canAct, next, "").Render(r.Context(), w)
	}
}

// renderTransfers re-renders the full table (used after post/cancel; no cursor).
func (s *Server) renderTransfers(w http.ResponseWriter, r *http.Request, flash string) {
	rows, next, canAct, err := s.transfersPage(r)
	if err != nil {
		http.Error(w, "transfers error", http.StatusInternalServerError)
		return
	}
	s.html(w)
	_ = template.TransferTable(rows, canAct, next, flash).Render(r.Context(), w)
}

func (s *Server) transfersPage(r *http.Request) ([]sqlc.SearchTransfersRow, string, bool, error) {
	ctx := r.Context()
	q := searchQ(r)
	ts, cid := pageCursor(r)
	limit := s.cfg.Server.DefaultPageLimit
	rows, err := s.pg.Queries.SearchTransfers(ctx, sqlc.SearchTransfersParams{
		Q: q, Cursor: ts, CursorID: cid, PageLimit: limit + 1,
	})
	if err != nil {
		s.log.Error("list transfers", "err", err)
		return nil, "", false, err
	}
	next := ""
	if int32(len(rows)) > limit {
		rows = rows[:limit]
		last := rows[limit-1]
		next = nextPageURL("/console/transfers/results", last.RequestedAt, last.ID, q)
	}
	canAct := false
	if su, ok := userFromContext(ctx); ok {
		canAct = canActOnMoney(su.Role)
	}
	return rows, next, canAct, nil
}

func (s *Server) consolePostTransfer(w http.ResponseWriter, r *http.Request) {
	su, id, ok := s.consoleActionContext(w, r)
	if !ok {
		return
	}
	if _, err := s.pg.Queries.PostTransfer(r.Context(), id); err != nil {
		s.renderTransfers(w, r, "Could not post: "+dbErrorMessage(err))
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
		s.renderTransfers(w, r, "Could not cancel: "+dbErrorMessage(err))
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
	limit := s.cfg.Server.DefaultPageLimit
	rows, err := s.pg.Queries.ListAuditLog(r.Context(), sqlc.ListAuditLogParams{
		Q: q, Cursor: ts, CursorID: cid, PageLimit: limit + 1,
	})
	if err != nil {
		s.log.Error("list audit", "err", err)
		http.Error(w, "audit error", http.StatusInternalServerError)
		return
	}
	next := ""
	if int32(len(rows)) > limit {
		rows = rows[:limit]
		last := rows[limit-1]
		next = nextPageURL("/console/audit/results", last.CreatedAt, last.ID, q)
	}
	s.html(w)
	if hasCursor(r) {
		_ = template.AuditItems(rows, next).Render(r.Context(), w)
	} else {
		_ = template.AuditRows(rows, next).Render(r.Context(), w)
	}
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
	limit := s.cfg.Server.DefaultPageLimit
	rows, err := s.pg.Queries.AccountStatement(ctx, sqlc.AccountStatementParams{
		AccountID: id, Cursor: ts, CursorID: cid, PageLimit: limit + 1,
	})
	if err != nil {
		mapDBError(w, err)
		return
	}
	next := ""
	if int32(len(rows)) > limit {
		rows = rows[:limit]
		last := rows[limit-1]
		next = nextPageURL("/console/accounts/"+id.String()+"/statement", last.PostedAt, last.ID, nil)
	}
	s.html(w)
	if hasCursor(r) {
		_ = template.StatementItems(rows, next).Render(ctx, w)
		return
	}
	acct, err := s.pg.Queries.GetAccount(ctx, id)
	if err != nil {
		mapDBError(w, err)
		return
	}
	_ = template.StatementView(acct, rows, next).Render(ctx, w)
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
		mapDBError(w, err)
		return
	}
	legs, err := s.pg.Queries.TransferLegs(ctx, id)
	if err != nil {
		mapDBError(w, err)
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
		s.renderTransferDetail(w, r, id, "Could not reverse: "+dbErrorMessage(err))
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
	rows, err := s.pg.Queries.ListPendingApprovals(ctx, s.cfg.Server.DefaultPageLimit)
	if err != nil {
		s.log.Error("list approvals", "err", err)
		http.Error(w, "approvals error", http.StatusInternalServerError)
		return
	}
	canAct := false
	if su, ok := userFromContext(ctx); ok {
		canAct = canApprove(su.Role)
	}
	s.html(w)
	_ = template.ApprovalRows(rows, canAct, flash).Render(ctx, w)
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
		s.renderApprovals(w, r, "Could not approve: "+dbErrorMessage(err))
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
		s.renderApprovals(w, r, "Could not reject: "+dbErrorMessage(err))
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
