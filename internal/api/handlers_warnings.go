package api

import (
	"net/http"

	"github.com/google/uuid"
	sqlc "github.com/minhtt159/bank0/internal/db/sqlc"
)

type warningAckReq struct {
	Category         string     `json:"category"`
	ReasonCode       string     `json:"reason_code"`
	Acknowledged     *bool      `json:"acknowledged"`
	DebitAccountID   *uuid.UUID `json:"debit_account_id"`
	CounterpartyIban string     `json:"counterparty_iban"`
	AmountMinor      *int64     `json:"amount_minor"`
	Device           string     `json:"device"`
}

// RecordWarningAck implements genclient.ServerInterface (POST /me/warning-acks):
// the CoP/VOP "warned and proceeded" liability evidence. Scoped to the subject;
// the DB rejects a debit account the caller doesn't own (42501 -> 403). The
// category CHECK lives in the table (23514 -> 422).
func (s *Server) RecordWarningAck(w http.ResponseWriter, r *http.Request) {
	subj, ok := clientSubject(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}
	var req warningAckReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Category == "" {
		writeError(w, http.StatusUnprocessableEntity, "invalid_input", "category is required")
		return
	}
	acked := true
	if req.Acknowledged != nil {
		acked = *req.Acknowledged
	}
	id, err := s.pg.Queries.RecordWarningAck(r.Context(), sqlc.RecordWarningAckParams{
		UserID:           subj,
		Category:         req.Category,
		ReasonCode:       req.ReasonCode,
		Acknowledged:     acked,
		DebitAccountID:   req.DebitAccountID,
		CounterpartyIban: req.CounterpartyIban,
		AmountMinor:      req.AmountMinor,
		Device:           req.Device,
	})
	if err != nil {
		s.mapDBError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id})
}
