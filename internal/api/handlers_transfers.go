package api

import (
	"net/http"
	"strconv"
	"time"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/google/uuid"
	"github.com/minhtt159/bank0/internal/api/genadmin"
	"github.com/minhtt159/bank0/internal/api/genclient"
	sqlc "github.com/minhtt159/bank0/internal/db/sqlc"
)

type createTransferReq struct {
	DebitAccount  string `json:"debit_account"`
	CreditAccount string `json:"credit_account"`
	AmountMinor   int64  `json:"amount_minor"`
	Description   string `json:"description"`
}

// CreateTransfer implements genclient.ServerInterface. Auto-posts by default;
// idempotent on the Idempotency-Key header (bound by the generated wrapper).
func (s *Server) CreateTransfer(w http.ResponseWriter, r *http.Request, params genclient.CreateTransferParams) {
	var req createTransferReq
	if !decodeJSON(w, r, &req) {
		return
	}
	debit, err := uuid.Parse(req.DebitAccount)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid debit_account")
		return
	}
	credit, err := uuid.Parse(req.CreditAccount)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid credit_account")
		return
	}
	// Client surface: you can only debit your own account.
	if subj, ok := clientSubject(r.Context()); ok {
		owner, err := s.pg.Queries.AccountOwner(r.Context(), debit)
		if err != nil {
			mapDBError(w, err)
			return
		}
		if !ownsAccount(subj, owner) {
			writeError(w, http.StatusForbidden, "forbidden", "debit account not owned by caller")
			return
		}
	}
	res, err := s.pg.Transfer(r.Context(), params.IdempotencyKey, debit, credit,
		req.AmountMinor, req.Description, sqlc.TransferKindTransfer)
	if err != nil {
		mapDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// GetTransfer implements both ServerInterfaces (shared, path-only).
func (s *Server) GetTransfer(w http.ResponseWriter, r *http.Request, id openapi_types.UUID) {
	if subj, ok := clientSubject(r.Context()); ok {
		o, err := s.pg.Queries.TransferOwners(r.Context(), uuid.UUID(id))
		if err != nil {
			mapDBError(w, err)
			return
		}
		if !ownsAccount(subj, o.DebitOwner) && !ownsAccount(subj, o.CreditOwner) {
			writeError(w, http.StatusNotFound, "not_found", "transfer not found")
			return
		}
	}
	t, err := s.pg.Queries.GetTransfer(r.Context(), uuid.UUID(id))
	if err != nil {
		mapDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

// requireTransferDebitOwner enforces (client surface only) that the caller owns
// the transfer's debit account. Returns false and writes the response on denial.
func (s *Server) requireTransferDebitOwner(w http.ResponseWriter, r *http.Request, id uuid.UUID) bool {
	subj, ok := clientSubject(r.Context())
	if !ok {
		return true // portal surface: operators act on the bank's behalf
	}
	o, err := s.pg.Queries.TransferOwners(r.Context(), id)
	if err != nil {
		mapDBError(w, err)
		return false
	}
	if !ownsAccount(subj, o.DebitOwner) {
		writeError(w, http.StatusNotFound, "not_found", "transfer not found")
		return false
	}
	return true
}

// ListPendingTransfers implements genadmin.ServerInterface.
func (s *Server) ListPendingTransfers(w http.ResponseWriter, r *http.Request, params genadmin.ListPendingTransfersParams) {
	s.respondPending(w, r, params.Cursor, params.Limit)
}

// listPendingJSON is the parent-router variant (parses query params itself) used
// so the static /transfers/pending route can be registered ahead of the client
// surface's /transfers/{id} in "all" mode. Behind requireSession.
func (s *Server) listPendingJSON(w http.ResponseWriter, r *http.Request) {
	var cursor *time.Time
	if c := r.URL.Query().Get("cursor"); c != "" {
		if t, err := time.Parse(time.RFC3339, c); err == nil {
			cursor = &t
		}
	}
	var limit *int
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil {
			limit = &n
		}
	}
	s.respondPending(w, r, cursor, limit)
}

func (s *Server) respondPending(w http.ResponseWriter, r *http.Request, cursor *time.Time, limit *int) {
	rows, err := s.pg.Queries.ListPendingTransfers(r.Context(), sqlc.ListPendingTransfersParams{
		Cursor:    cursor,
		PageLimit: s.limitOr(limit),
	})
	if err != nil {
		mapDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

// PostTransfer implements genclient.ServerInterface.
func (s *Server) PostTransfer(w http.ResponseWriter, r *http.Request, id openapi_types.UUID) {
	if !s.requireTransferDebitOwner(w, r, uuid.UUID(id)) {
		return
	}
	status, err := s.pg.Queries.PostTransfer(r.Context(), uuid.UUID(id))
	if err != nil {
		mapDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "status": status})
}

type reasonReq struct {
	Reason string `json:"reason"`
}

// CancelTransfer implements genclient.ServerInterface.
func (s *Server) CancelTransfer(w http.ResponseWriter, r *http.Request, id openapi_types.UUID) {
	if !s.requireTransferDebitOwner(w, r, uuid.UUID(id)) {
		return
	}
	var req reasonReq
	decodeOptionalJSON(r, &req)
	status, err := s.pg.Queries.CancelTransfer(r.Context(), sqlc.CancelTransferParams{
		ID: uuid.UUID(id), Reason: req.Reason,
	})
	if err != nil {
		mapDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "status": status})
}

// ReverseTransfer implements genadmin.ServerInterface.
func (s *Server) ReverseTransfer(w http.ResponseWriter, r *http.Request, id openapi_types.UUID, params genadmin.ReverseTransferParams) {
	if _, ok := s.requireRole(w, r, canActOnMoney); !ok {
		return
	}
	var req reasonReq
	if !decodeJSON(w, r, &req) {
		return
	}
	reversalID, err := s.pg.Queries.ReverseTransfer(r.Context(), sqlc.ReverseTransferParams{
		ID: uuid.UUID(id), IdempotencyKey: params.IdempotencyKey, Reason: req.Reason,
	})
	if err != nil {
		mapDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"reversal_id": reversalID})
}
