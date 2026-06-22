package api

import (
	"net/http"

	"github.com/google/uuid"

	"github.com/minhtt159/bank0/internal/api/genclient"
	"github.com/minhtt159/bank0/internal/db"
)

// SuggestTransferDestinations implements genclient.ServerInterface: the guided-
// transfer "mule menu" (spec-guided-transfer-mule-menu.md). Read-only — it only
// NAMES up to 3 candidate destinations (other users', random, optionally including a
// short-listed mule); the transfer itself still goes through POST /transfers
// (idempotency-key) unchanged. It never leaks more than /beneficiaries/resolve
// (masked owner name + iban + account_id; no balance, no full name, no owner id).
// Always 200: an empty array means "no candidate", and the client falls back to the
// caller's own account.
func (s *Server) SuggestTransferDestinations(w http.ResponseWriter, r *http.Request, params genclient.SuggestTransferDestinationsParams) {
	subj, ok := s.clientSubjectOr401(w, r)
	if !ok {
		return
	}
	// from_account ownership is checked FIRST, so the endpoint can't be used as an
	// oracle over accounts the caller doesn't own (403 purely from the ownership
	// check, before any resolve).
	var from *uuid.UUID
	if params.FromAccount != nil {
		owner, err := s.pg.Queries.AccountOwner(r.Context(), uuid.UUID(*params.FromAccount))
		if err != nil {
			s.mapDBError(w, r, err)
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
	candidates, err := s.pg.SuggestTransferDestinations(r.Context(), subj, from, amount)
	if err != nil {
		s.mapDBError(w, r, err)
		return
	}
	// Wrap in {"options": [...]} (the spec's deliberate one-time envelope so future
	// fields stay additive). Coerce nil -> [] so an empty menu is `{"options": []}`,
	// the client's signal to fall back to the caller's own account.
	if candidates == nil {
		candidates = []db.TransferSuggestion{}
	}
	writeJSON(w, http.StatusOK, struct {
		Options []db.TransferSuggestion `json:"options"`
	}{Options: candidates})
}
