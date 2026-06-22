package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/minhtt159/bank0/internal/db"
	sqlc "github.com/minhtt159/bank0/internal/db/sqlc"
	"github.com/minhtt159/bank0/internal/iban"
	"github.com/minhtt159/bank0/internal/money"
	template "github.com/minhtt159/bank0/web/template"
)

// ---- accounts (main panel + rail actions; re-render the owner's detail) --

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
	// Resolve the owning user for the re-render.
	ownerPtr, err := s.pg.Queries.AccountOwner(r.Context(), acctID)
	if err != nil {
		s.mapDBError(w, r, err)
		return db.SessionUser{}, uuid.Nil, uuid.Nil, false
	}
	if ownerPtr == nil {
		writeError(w, http.StatusBadRequest, "bad_request", "account has no owner")
		return db.SessionUser{}, uuid.Nil, uuid.Nil, false
	}
	owner = *ownerPtr
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

func (s *Server) consoleStatement(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid account id")
		return
	}
	ts, cid := pageCursor(r)
	limit := s.consolePageLimit(r)
	// The client GetAccountLedger query is a superset of the old console-only
	// AccountStatement (same columns/cursor/order); the extra filter nargs left nil
	// degenerate to the unfiltered statement (DB-6 dedup).
	rows, err := s.pg.Queries.GetAccountLedger(ctx, sqlc.GetAccountLedgerParams{
		AccountID: id, Cursor: ts, CursorID: cid, PageLimit: limit + 1,
	})
	if err != nil {
		s.mapDBError(w, r, err)
		return
	}
	rows, lastTs, lastID, hasMore := paginate(rows, limit, func(e sqlc.GetAccountLedgerRow) (time.Time, uuid.UUID) {
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
