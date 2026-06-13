package api

import (
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/minhtt159/bank0/internal/api/genclient"
)

// SuggestTransferDestination implements genclient.ServerInterface: the guided-
// transfer demo endpoint. Read-only — it only NAMES a destination; the transfer
// itself still goes through POST /transfers (idempotency-key) unchanged. It never
// leaks more than /beneficiaries/resolve (masked owner name + iban + account_id;
// no balance, no full name, no owner id). See
// docs/specs/spec-guided-transfer-suggestion.md.
func (s *Server) SuggestTransferDestination(w http.ResponseWriter, r *http.Request, params genclient.SuggestTransferDestinationParams) {
	subj, ok := clientSubject(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}
	// from_account ownership is checked FIRST, so the endpoint can't be used as an
	// oracle over accounts the caller doesn't own (403 purely from the ownership
	// check, before any resolve — no 403-vs-204 timing distinction on foreign ids).
	var from *uuid.UUID
	if params.FromAccount != nil {
		owner, err := s.pg.Queries.AccountOwner(r.Context(), uuid.UUID(*params.FromAccount))
		if err != nil {
			mapDBError(w, err)
			return
		}
		if !ownsAccount(subj, owner) {
			writeError(w, http.StatusForbidden, "forbidden", "from_account not owned by caller")
			return
		}
		f := uuid.UUID(*params.FromAccount)
		from = &f
	}
	var amount int64
	if params.AmountMinor != nil {
		amount = *params.AmountMinor
	}
	sg, err := s.pg.SuggestTransferDestination(r.Context(), subj, from, amount)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			w.WriteHeader(http.StatusNoContent) // nothing eligible -> client falls back to manual
			return
		}
		mapDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sg)
}
