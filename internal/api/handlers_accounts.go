package api

import (
	"net/http"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/google/uuid"
	"github.com/minhtt159/bank0/internal/api/genadmin"
	"github.com/minhtt159/bank0/internal/api/genclient"
	"github.com/minhtt159/bank0/internal/iban"
	sqlc "github.com/minhtt159/bank0/internal/db/sqlc"
)

type createAccountReq struct {
	UserID             string `json:"user_id"`
	Iban               string `json:"iban"`
	Pin                string `json:"pin"`
	TransferLimitMinor int64  `json:"transfer_limit_minor"`
}

// CreateAccount implements genadmin.ServerInterface.
func (s *Server) CreateAccount(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireRole(w, r, canActOnMoney); !ok {
		return
	}
	var req createAccountReq
	if !decodeJSON(w, r, &req) {
		return
	}
	userID, err := uuid.Parse(req.UserID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid user_id")
		return
	}
	if req.TransferLimitMinor == 0 {
		req.TransferLimitMinor = 50000
	}
	normIban := iban.Normalize(req.Iban)
	if !iban.IsValid(normIban) {
		writeError(w, http.StatusUnprocessableEntity, "invalid_iban", "iban failed checksum/format validation")
		return
	}
	id, err := s.pg.Queries.CreateAccount(r.Context(), sqlc.CreateAccountParams{
		UserID:             userID,
		Iban:               normIban,
		Pin:                req.Pin,
		TransferLimitMinor: req.TransferLimitMinor,
	})
	if err != nil {
		mapDBError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id})
}

// GetAccount implements both ServerInterfaces (shared, path-only).
func (s *Server) GetAccount(w http.ResponseWriter, r *http.Request, id openapi_types.UUID) {
	a, err := s.pg.Queries.GetAccount(r.Context(), uuid.UUID(id))
	if err != nil {
		mapDBError(w, err)
		return
	}
	if subj, ok := clientSubject(r.Context()); ok && !ownsAccount(subj, a.UserID) {
		writeError(w, http.StatusNotFound, "not_found", "account not found")
		return
	}
	writeJSON(w, http.StatusOK, a)
}

// ListUserAccounts implements both ServerInterfaces (shared, path-only).
func (s *Server) ListUserAccounts(w http.ResponseWriter, r *http.Request, id openapi_types.UUID) {
	if subj, ok := clientSubject(r.Context()); ok && uuid.UUID(id) != subj {
		writeError(w, http.StatusNotFound, "not_found", "not found")
		return
	}
	rows, err := s.pg.Queries.ListAccountsByUser(r.Context(), uuid.UUID(id))
	if err != nil {
		mapDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

// GetAccountLedger implements genclient.ServerInterface.
func (s *Server) GetAccountLedger(w http.ResponseWriter, r *http.Request, id openapi_types.UUID, params genclient.GetAccountLedgerParams) {
	if subj, ok := clientSubject(r.Context()); ok {
		owner, err := s.pg.Queries.AccountOwner(r.Context(), uuid.UUID(id))
		if err != nil {
			mapDBError(w, err)
			return
		}
		if !ownsAccount(subj, owner) {
			writeError(w, http.StatusNotFound, "not_found", "account not found")
			return
		}
	}
	var direction *string
	if params.Direction != nil {
		d := string(*params.Direction)
		direction = &d
	}
	rows, err := s.pg.Queries.GetAccountLedger(r.Context(), sqlc.GetAccountLedgerParams{
		AccountID: uuid.UUID(id),
		Cursor:    params.Cursor,
		CursorID:  params.CursorId, // openapi_types.UUID is an alias of uuid.UUID
		FromTs:    params.From,
		ToTs:      params.To,
		Direction: direction,
		MinMinor:  params.MinMinor,
		MaxMinor:  params.MaxMinor,
		Q:         params.Q,
		PageLimit: s.limitOr(params.Limit),
	})
	if err != nil {
		mapDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

type amountReq struct {
	AmountMinor int64  `json:"amount_minor"`
	Description string `json:"description"`
}

// Deposit implements genadmin.ServerInterface. Money above the maker-checker
// threshold should route to the approvals queue (future work).
func (s *Server) Deposit(w http.ResponseWriter, r *http.Request, id openapi_types.UUID, params genadmin.DepositParams) {
	if _, ok := s.requireRole(w, r, canActOnMoney); !ok {
		return
	}
	var req amountReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Description == "" {
		req.Description = "Deposit"
	}
	tid, err := s.pg.Queries.Deposit(r.Context(), sqlc.DepositParams{
		IdempotencyKey: params.IdempotencyKey,
		AccountID:      uuid.UUID(id),
		AmountMinor:    req.AmountMinor,
		Description:    req.Description,
	})
	if err != nil {
		mapDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"transfer_id":       tid,
		"requires_approval": req.AmountMinor > s.cfg.Admin.MakerCheckerThresholdMinor,
	})
}

// Withdraw implements genadmin.ServerInterface.
func (s *Server) Withdraw(w http.ResponseWriter, r *http.Request, id openapi_types.UUID, params genadmin.WithdrawParams) {
	if _, ok := s.requireRole(w, r, canActOnMoney); !ok {
		return
	}
	var req amountReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Description == "" {
		req.Description = "Withdrawal"
	}
	tid, err := s.pg.Queries.Withdraw(r.Context(), sqlc.WithdrawParams{
		IdempotencyKey: params.IdempotencyKey,
		AccountID:      uuid.UUID(id),
		AmountMinor:    req.AmountMinor,
		Description:    req.Description,
	})
	if err != nil {
		mapDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"transfer_id":       tid,
		"requires_approval": req.AmountMinor > s.cfg.Admin.MakerCheckerThresholdMinor,
	})
}

type statusReq struct {
	Status string `json:"status"`
}

// SetAccountStatus implements genadmin.ServerInterface.
func (s *Server) SetAccountStatus(w http.ResponseWriter, r *http.Request, id openapi_types.UUID) {
	if _, ok := s.requireRole(w, r, canActOnMoney); !ok {
		return
	}
	var req statusReq
	if !decodeJSON(w, r, &req) {
		return
	}
	switch sqlc.AccountStatus(req.Status) {
	case sqlc.AccountStatusActive, sqlc.AccountStatusFrozen, sqlc.AccountStatusClosed:
	default:
		writeError(w, http.StatusBadRequest, "bad_request", "invalid status")
		return
	}
	if err := s.pg.Queries.SetAccountStatus(r.Context(), sqlc.SetAccountStatusParams{
		AccountID: uuid.UUID(id), Status: sqlc.AccountStatus(req.Status),
	}); err != nil {
		mapDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "status": req.Status})
}
