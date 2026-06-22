package api

import (
	"net/http"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/google/uuid"
	"github.com/minhtt159/bank0/internal/api/genadmin"
	"github.com/minhtt159/bank0/internal/api/genclient"
	sqlc "github.com/minhtt159/bank0/internal/db/sqlc"
)

// Customer self-service (spec-customer-account-opening): open a second account
// (server-minted IBAN) and request transfer-limit changes, which operators
// resolve through the maker-checker queue. Limits are never self-applied.

type openAccountReq struct {
	Kind string `json:"kind"`
}

// OpenMyAccount implements genclient.ServerInterface (POST /me/accounts).
// Ownership is implicit: the account is opened for the JWT subject — there is
// no user_id in the request, so a caller can never open for someone else.
func (s *Server) OpenMyAccount(w http.ResponseWriter, r *http.Request, params genclient.OpenMyAccountParams) {
	subj, ok := s.clientSubjectOr401(w, r)
	if !ok {
		return
	}
	var req openAccountReq
	decodeOptionalJSON(r, &req)
	if req.Kind != "" && req.Kind != "customer" {
		// savings/pots is roadmap — fail loud rather than silently opening 'customer'.
		writeError(w, http.StatusUnprocessableEntity, "unsupported_kind", "only kind 'customer' is supported")
		return
	}

	accountID, wasReplay, err := s.pg.OpenCustomerAccount(r.Context(), params.IdempotencyKey, subj)
	if err != nil {
		s.mapDBError(w, r, err) // cap -> 409 account_limit (mapDBError)
		return
	}
	if wasReplay {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	acct, err := s.pg.Queries.GetAccount(r.Context(), accountID)
	if err != nil {
		s.mapDBError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, acct)
}

type limitRequestReq struct {
	TransferLimitMinor int64  `json:"transfer_limit_minor"`
	Reason             string `json:"reason"`
}

// RequestLimitChange implements genclient.ServerInterface
// (POST /accounts/{id}/limit-requests). 403 on a non-owned account (matches the
// transfer-debit convention: the caller can see the account exists in their own
// listings only if it's theirs).
func (s *Server) RequestLimitChange(w http.ResponseWriter, r *http.Request, id openapi_types.UUID) {
	subj, ok := s.clientSubjectOr401(w, r)
	if !ok {
		return
	}
	owner, err := s.pg.Queries.AccountOwner(r.Context(), uuid.UUID(id))
	if err != nil {
		s.mapDBError(w, r, err) // no rows -> 404
		return
	}
	if !ownsAccount(subj, owner) {
		writeError(w, http.StatusForbidden, "forbidden", "you do not own this account")
		return
	}
	var req limitRequestReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.TransferLimitMinor < 0 {
		writeError(w, http.StatusUnprocessableEntity, "invalid_input", "transfer_limit_minor must be >= 0")
		return
	}
	reqID, err := s.pg.Queries.RequestLimitChange(r.Context(), sqlc.RequestLimitChangeParams{
		AccountID: uuid.UUID(id), Maker: subj, NewLimit: req.TransferLimitMinor, Reason: req.Reason,
	})
	if err != nil {
		s.mapDBError(w, r, err) // unchanged limit -> 422
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"request_id":            reqID,
		"account_id":            id,
		"requested_limit_minor": req.TransferLimitMinor,
		"status":                "pending",
	})
}

// ListLimitRequests implements genadmin.ServerInterface. Any staff session may
// view the queue (like the pending-transfers queue); only admins resolve.
func (s *Server) ListLimitRequests(w http.ResponseWriter, r *http.Request, params genadmin.ListLimitRequestsParams) {
	rows, err := s.pg.Queries.ListLimitRequests(r.Context(), sqlc.ListLimitRequestsParams{
		Cursor:    params.Cursor,
		PageLimit: s.limitOr(params.Limit),
	})
	if err != nil {
		s.mapDBError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

// ApproveLimitRequest implements genadmin.ServerInterface. 4-eyes lives in the
// DB (approve_limit_change): approving your own request -> 42501 -> 403;
// already handled -> 409.
func (s *Server) ApproveLimitRequest(w http.ResponseWriter, r *http.Request, id openapi_types.UUID) {
	actor, ok := s.requireRole(w, r, canApprove)
	if !ok {
		return
	}
	accountID, err := s.pg.Queries.ApproveLimitChange(r.Context(), sqlc.ApproveLimitChangeParams{
		RequestID: uuid.UUID(id), Approver: actor.UserID,
	})
	if err != nil {
		s.mapDBError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": accountID, "status": "applied"})
}

// RejectLimitRequest implements genadmin.ServerInterface.
func (s *Server) RejectLimitRequest(w http.ResponseWriter, r *http.Request, id openapi_types.UUID) {
	actor, ok := s.requireRole(w, r, canApprove)
	if !ok {
		return
	}
	var req reasonReq
	decodeOptionalJSON(r, &req)
	accountID, err := s.pg.Queries.RejectLimitChange(r.Context(), sqlc.RejectLimitChangeParams{
		RequestID: uuid.UUID(id), Approver: actor.UserID, Reason: req.Reason,
	})
	if err != nil {
		s.mapDBError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": accountID, "status": "rejected"})
}
