package api

import (
	"net/http"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/google/uuid"
	"github.com/minhtt159/bank0/internal/api/genclient"
	"github.com/minhtt159/bank0/internal/iban"
	sqlc "github.com/minhtt159/bank0/internal/db/sqlc"
)

// Beneficiaries are the customer app's saved payees (docs/07). Every operation is
// scoped to the JWT subject; they are a lookup/directory only — money still moves
// through createTransfer, which independently enforces debit-account ownership.

// ListBeneficiaries implements genclient.ServerInterface.
func (s *Server) ListBeneficiaries(w http.ResponseWriter, r *http.Request) {
	subj, ok := clientSubject(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}
	rows, err := s.pg.Queries.ListBeneficiaries(r.Context(), subj)
	if err != nil {
		s.mapDBError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rows) // writeJSON coerces a nil slice to []
}

type addBeneficiaryReq struct {
	Label string `json:"label"`
	Iban  string `json:"iban"`
}

// AddBeneficiary implements genclient.ServerInterface: resolve an IBAN to an
// account and save it for the caller. Returns the created beneficiary.
func (s *Server) AddBeneficiary(w http.ResponseWriter, r *http.Request) {
	subj, ok := clientSubject(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}
	var req addBeneficiaryReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Label == "" || req.Iban == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "label and iban are required")
		return
	}
	if !iban.IsValid(req.Iban) {
		writeError(w, http.StatusUnprocessableEntity, "invalid_iban", "iban failed checksum/format validation")
		return
	}
	id, err := s.pg.Queries.AddBeneficiary(r.Context(), sqlc.AddBeneficiaryParams{
		Owner: subj, Label: req.Label, Iban: iban.Normalize(req.Iban),
	})
	if err != nil {
		s.mapDBError(w, r, err)
		return
	}
	b, err := s.pg.Queries.GetBeneficiary(r.Context(), sqlc.GetBeneficiaryParams{ID: id, Owner: subj})
	if err != nil {
		s.mapDBError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, b)
}

// ResolveBeneficiary implements genclient.ServerInterface: confirmation-of-payee
// preview for an IBAN before saving it (masked owner name, no balance).
func (s *Server) ResolveBeneficiary(w http.ResponseWriter, r *http.Request, params genclient.ResolveBeneficiaryParams) {
	if _, ok := clientSubject(r.Context()); !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}
	if params.Iban == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "iban is required")
		return
	}
	if !iban.IsValid(params.Iban) {
		writeError(w, http.StatusUnprocessableEntity, "invalid_iban", "iban failed checksum/format validation")
		return
	}
	a, err := s.pg.ResolveAccountByIban(r.Context(), iban.Normalize(params.Iban))
	if err != nil {
		s.mapDBError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

// DeleteBeneficiary implements genclient.ServerInterface: scoped removal.
func (s *Server) DeleteBeneficiary(w http.ResponseWriter, r *http.Request, id openapi_types.UUID) {
	subj, ok := clientSubject(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}
	if err := s.pg.Queries.DeleteBeneficiary(r.Context(), sqlc.DeleteBeneficiaryParams{
		Owner: subj, ID: uuid.UUID(id),
	}); err != nil {
		s.mapDBError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
