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

func validTransferStatus(s sqlc.TransferStatus) bool {
	switch s {
	case sqlc.TransferStatusPending, sqlc.TransferStatusHeld, sqlc.TransferStatusUnderReview,
		sqlc.TransferStatusPosted, sqlc.TransferStatusFailed,
		sqlc.TransferStatusCanceled, sqlc.TransferStatusReversed:
		return true
	}
	return false
}

func validTransferKind(k sqlc.TransferKind) bool {
	switch k {
	case sqlc.TransferKindTransfer, sqlc.TransferKindDeposit, sqlc.TransferKindWithdrawal,
		sqlc.TransferKindReversal, sqlc.TransferKindFee, sqlc.TransferKindAdjustment:
		return true
	}
	return false
}

// ListMyTransfers implements genclient.ServerInterface: the caller's cross-account
// transfer history, newest first. Ownership (caller owns debit OR credit) and paging
// live in SQL; the response is a bare array with a composite (requested_at, id)
// keyset cursor — pass the last row's requested_at as cursor + its id as cursor_id.
// Read-only, no idempotency. See docs/specs/spec-list-my-transfers.md.
func (s *Server) ListMyTransfers(w http.ResponseWriter, r *http.Request, params genclient.ListMyTransfersParams) {
	subj, ok := s.clientSubjectOr401(w, r)
	if !ok {
		return
	}
	q := sqlc.ListMyTransfersParams{
		Subject:   subj,
		Cursor:    params.Cursor,
		CursorID:  params.CursorId, // openapi_types.UUID is an alias of uuid.UUID
		FromTs:    params.From,
		ToTs:      params.To,
		Q:         params.Q,
		PageLimit: s.limitOr(params.Limit),
	}
	if params.Status != nil {
		st := sqlc.TransferStatus(*params.Status)
		if !validTransferStatus(st) {
			writeError(w, http.StatusBadRequest, "bad_request", "invalid status")
			return
		}
		q.Status = sqlc.NullTransferStatus{TransferStatus: st, Valid: true}
	}
	if params.Kind != nil {
		k := sqlc.TransferKind(*params.Kind)
		if !validTransferKind(k) {
			writeError(w, http.StatusBadRequest, "bad_request", "invalid kind")
			return
		}
		q.Kind = sqlc.NullTransferKind{TransferKind: k, Valid: true}
	}
	if params.Direction != nil {
		d := string(*params.Direction)
		if d != "out" && d != "in" {
			writeError(w, http.StatusBadRequest, "bad_request", "invalid direction")
			return
		}
		q.Dir = &d
	}
	if params.From != nil && params.To != nil && !params.From.Before(*params.To) {
		writeError(w, http.StatusBadRequest, "bad_request", "from must be before to")
		return
	}
	rows, err := s.pg.Queries.ListMyTransfers(r.Context(), q)
	if err != nil {
		s.mapDBError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rows) // writeJSON coerces a nil slice -> []
}

type createTransferReq struct {
	DebitAccount  string  `json:"debit_account"`
	CreditAccount string  `json:"credit_account"`
	AmountMinor   int64   `json:"amount_minor"`
	Description   string  `json:"description"`
	EndToEndID    *string `json:"end_to_end_id,omitempty"` // optional ISO 20022 originator reference
}

// CreateTransfer implements genclient.ServerInterface. Auto-posts by default;
// idempotent on the Idempotency-Key header (bound by the generated wrapper).
func (s *Server) CreateTransfer(w http.ResponseWriter, r *http.Request, params genclient.CreateTransferParams) {
	subj, ok := s.clientSubjectOr401(w, r)
	if !ok {
		return
	}
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
	// Step-up gate (SCA + dynamic linking, Recs 13/14/15): an MFA-enabled caller
	// on a gated transfer must present a FRESH otp factor whose txn_link commits
	// to THIS exact (debit, credit, amount) — a generic fresh OTP no longer
	// authorizes any payment in the window (PSD2 RTS Art. 5 / WYSIWYS). Gated =
	// high value OR new payee OR the server-side TRA seam scores 'high'. Runs
	// BEFORE client_transfer, so a rejected step-up writes nothing and never
	// claims the idempotency key — the client verifies with the link and retries
	// with the SAME key. Users without MFA are not gated (they could never
	// satisfy it); the per-transfer limit + maker-checker still apply.
	if claims, ok := clientClaimsFrom(r.Context()); ok &&
		!(claims.hasFreshOTP(s.cfg.Auth.StepUpMaxAge) &&
			claims.TxnLink == transferLinkHash(debit, credit, req.AmountMinor)) {
		enabled, err := s.pg.MFAEnabled(r.Context(), subj)
		if err != nil {
			s.mapDBError(w, r, err)
			return
		}
		if enabled {
			gated := s.cfg.Auth.StepUpLimitMinor > 0 && req.AmountMinor >= s.cfg.Auth.StepUpLimitMinor
			if !gated {
				known, err := s.pg.Queries.IsKnownPayee(r.Context(), sqlc.IsKnownPayeeParams{Owner: subj, Credit: credit})
				if err != nil {
					s.mapDBError(w, r, err)
					return
				}
				gated = !known
			}
			if !gated {
				// TRA seam (Rec 15): the authoritative risk decision ORs in.
				risk, err := s.pg.AssessTransferRisk(r.Context(), subj, debit, credit, req.AmountMinor)
				if err != nil {
					s.mapDBError(w, r, err)
					return
				}
				gated = risk.Band == "high"
			}
			if gated {
				writeError(w, http.StatusForbidden, "step_up_required",
					"this transfer requires re-verification linked to this exact payment (high value, new payee, or elevated risk)")
				return
			}
		}
	}
	// Debit-account ownership is enforced inside client_transfer (one round trip, in
	// the DB) — non-ownership raises 42501 -> 403. No separate probe.
	res, err := s.pg.ClientTransfer(r.Context(), subj, params.IdempotencyKey, debit, credit,
		req.AmountMinor, req.Description, req.EndToEndID)
	if err != nil {
		s.mapDBError(w, r, err)
		return
	}
	if res.WasReplay {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	writeJSON(w, http.StatusOK, res)
}

type transferIntentReq struct {
	DebitAccount  string `json:"debit_account"`
	CreditAccount string `json:"credit_account"`
	AmountMinor   int64  `json:"amount_minor"`
}

// TransferIntent implements genclient.ServerInterface: the Rec 22 read-only
// decision preflight. It runs the SAME server-side risk evaluation POST /transfers
// applies, but reserves nothing, posts nothing, and writes no row. The numeric risk
// score is never surfaced. Debit-account ownership is enforced in the DB
// (evaluate_transfer -> 42501 -> 403), so a foreign debit is rejected there.
func (s *Server) TransferIntent(w http.ResponseWriter, r *http.Request) {
	subj, ok := s.clientSubjectOr401(w, r)
	if !ok {
		return
	}
	var req transferIntentReq
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
	eval, err := s.pg.EvaluateTransfer(r.Context(), subj, debit, credit, req.AmountMinor, s.cfg.Auth.StepUpLimitMinor)
	if err != nil {
		s.mapDBError(w, r, err)
		return
	}
	// Mirror the CreateTransfer step-up gate exactly: the DB flags 'step_up' whenever
	// the step-up axis fires (high value / new payee / high band), but the caller is
	// only ACTUALLY gated when they have MFA enrolled AND lack a fresh OTP whose
	// txn_link commits to THIS exact (debit, credit, amount). If they wouldn't be
	// gated, the preflight downgrades step_up -> allow (and drops the method) so the
	// client isn't told to step up for a payment it can already make.
	if eval.Decision == "step_up" {
		downgrade := false
		if claims, ok := clientClaimsFrom(r.Context()); ok &&
			claims.hasFreshOTP(s.cfg.Auth.StepUpMaxAge) &&
			claims.TxnLink == transferLinkHash(debit, credit, req.AmountMinor) {
			downgrade = true // already re-verified for this exact payment
		}
		if !downgrade {
			enabled, err := s.pg.MFAEnabled(r.Context(), subj)
			if err != nil {
				s.mapDBError(w, r, err)
				return
			}
			if !enabled {
				downgrade = true // no second factor -> could never be gated
			}
		}
		if downgrade {
			eval.Decision = "allow"
			eval.StepUpMethod = ""
		}
	}

	reasons := eval.ReasonCodes
	if reasons == nil {
		reasons = []string{} // contract: reason_codes is always an array, never null
	}
	out := genclient.TransferIntent{
		Decision:    genclient.TransferIntentDecision(eval.Decision),
		RiskBand:    genclient.TransferIntentRiskBand(eval.RiskBand),
		ReasonCodes: reasons,
	}
	if eval.StepUpMethod != "" {
		out.StepUpMethod = &eval.StepUpMethod
	}
	// A warning is surfaced only when a warning rule actually matched.
	if eval.RuleID != nil {
		sev := genclient.TransferWarningSeverity(eval.Severity)
		cool := int(eval.CoolingOffSeconds)
		id := *eval.RuleID
		out.Warning = &genclient.TransferWarning{
			WarningId:         &id,
			Category:          &eval.Category,
			Severity:          &sev,
			Headline:          &eval.Headline,
			Body:              &eval.Body,
			RequiredAck:       &eval.RequiredAck,
			CoolingOffSeconds: &cool,
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// ConfirmTransfer implements genclient.ServerInterface: the customer releases their
// OWN held transfer (Rec 22 cooling-off), posting it. Ownership + state transitions
// live in client_confirm_transfer (a foreign/unknown transfer -> 404; not-held /
// under_review / expired window -> 409; already-posted is an idempotent no-op).
func (s *Server) ConfirmTransfer(w http.ResponseWriter, r *http.Request, id openapi_types.UUID) {
	subj, ok := s.clientSubjectOr401(w, r)
	if !ok {
		return
	}
	status, err := s.pg.Queries.ClientConfirmTransfer(r.Context(), sqlc.ClientConfirmTransferParams{
		CallerSubject: subj, ID: uuid.UUID(id),
	})
	if err != nil {
		s.mapDBError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "status": status})
}

// GetTransfer implements both ServerInterfaces (shared, path-only).
func (s *Server) GetTransfer(w http.ResponseWriter, r *http.Request, id openapi_types.UUID) {
	if subj, ok := clientSubject(r.Context()); ok {
		o, err := s.pg.Queries.TransferOwners(r.Context(), uuid.UUID(id))
		if err != nil {
			s.mapDBError(w, r, err)
			return
		}
		if !ownsAccount(subj, o.DebitOwner) && !ownsAccount(subj, o.CreditOwner) {
			writeError(w, http.StatusNotFound, "not_found", "transfer not found")
			return
		}
	}
	t, err := s.pg.Queries.GetTransfer(r.Context(), uuid.UUID(id))
	if err != nil {
		s.mapDBError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
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
		s.mapDBError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

// PostTransfer implements genclient.ServerInterface. Debit-account ownership is
// enforced inside client_post_transfer (one round trip); a transfer the caller
// doesn't own raises 'not found' -> 404, hiding existence.
func (s *Server) PostTransfer(w http.ResponseWriter, r *http.Request, id openapi_types.UUID) {
	subj, ok := s.clientSubjectOr401(w, r)
	if !ok {
		return
	}
	status, err := s.pg.Queries.ClientPostTransfer(r.Context(), sqlc.ClientPostTransferParams{
		CallerSubject: subj, ID: uuid.UUID(id),
	})
	if err != nil {
		s.mapDBError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "status": status})
}

type reasonReq struct {
	Reason string `json:"reason"`
}

// CancelTransfer implements genclient.ServerInterface. Ownership enforced in the DB
// (client_cancel_transfer); a transfer the caller doesn't own -> 404.
func (s *Server) CancelTransfer(w http.ResponseWriter, r *http.Request, id openapi_types.UUID) {
	subj, ok := s.clientSubjectOr401(w, r)
	if !ok {
		return
	}
	var req reasonReq
	decodeOptionalJSON(r, &req)
	status, err := s.pg.Queries.ClientCancelTransfer(r.Context(), sqlc.ClientCancelTransferParams{
		CallerSubject: subj, ID: uuid.UUID(id), Reason: req.Reason,
	})
	if err != nil {
		s.mapDBError(w, r, err)
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
		s.mapDBError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"reversal_id": reversalID})
}
