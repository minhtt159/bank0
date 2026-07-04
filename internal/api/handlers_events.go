package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/minhtt159/bank0/internal/api/genclient"
	sqlc "github.com/minhtt159/bank0/internal/db/sqlc"
)

// Notification feed (spec-notifications-events, phase 1: poll). Ownership is the
// user_id = subject predicate — the feed can only ever contain the caller's own
// rows, so there is no per-row 404 and no IDOR surface.

func validEventType(t sqlc.EventType) bool {
	switch t {
	case sqlc.EventTypeTransferposted, sqlc.EventTypePaymentincoming,
		sqlc.EventTypeDevicenew, sqlc.EventTypeDisputeupdated:
		return true
	}
	return false
}

// eventDTO re-shapes sqlc.Event so the JSONB payload reaches the client as an
// object (sqlc scans JSONB into []byte, which encoding/json would base64).
type eventDTO struct {
	ID                uuid.UUID       `json:"id"`
	Type              string          `json:"type"`
	Title             string          `json:"title"`
	Body              string          `json:"body"`
	RelatedTransferID *uuid.UUID      `json:"related_transfer_id,omitempty"`
	RelatedAccountID  *uuid.UUID      `json:"related_account_id,omitempty"`
	Data              json.RawMessage `json:"data"`
	ReadAt            *time.Time      `json:"read_at,omitempty"`
	CreatedAt         time.Time       `json:"created_at"`
}

func toEventDTO(e sqlc.Event) eventDTO {
	data := json.RawMessage(e.Data)
	if len(data) == 0 {
		data = json.RawMessage("{}")
	}
	return eventDTO{
		ID: e.ID, Type: string(e.Type), Title: e.Title, Body: e.Body,
		RelatedTransferID: e.RelatedTransferID, RelatedAccountID: e.RelatedAccountID,
		Data: data, ReadAt: e.ReadAt, CreatedAt: e.CreatedAt,
	}
}

// ListMyEvents implements genclient.ServerInterface (GET /me/events).
// Bare array, newest first; paginate with cursor + cursor_id from the last row.
func (s *Server) ListMyEvents(w http.ResponseWriter, r *http.Request, params genclient.ListMyEventsParams) {
	subj, ok := clientSubject(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}
	q := sqlc.ListMyEventsParams{
		UserID:     subj,
		Cursor:     params.Cursor,
		CursorID:   params.CursorId,
		UnreadOnly: params.UnreadOnly != nil && *params.UnreadOnly,
		PageLimit:  s.limitOr(params.Limit),
	}
	if params.Type != nil {
		et := sqlc.EventType(*params.Type)
		if !validEventType(et) {
			writeError(w, http.StatusBadRequest, "bad_request", "invalid type")
			return
		}
		q.Type = sqlc.NullEventType{EventType: et, Valid: true}
	}
	rows, err := s.pg.Queries.ListMyEvents(r.Context(), q)
	if err != nil {
		s.mapDBError(w, r, err)
		return
	}
	items := make([]eventDTO, 0, len(rows))
	for _, row := range rows {
		items = append(items, toEventDTO(row))
	}
	writeJSON(w, http.StatusOK, items)
}

// CountUnreadEvents implements genclient.ServerInterface (GET /me/events/unread).
func (s *Server) CountUnreadEvents(w http.ResponseWriter, r *http.Request) {
	subj, ok := clientSubject(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}
	n, err := s.pg.Queries.CountMyUnreadEvents(r.Context(), subj)
	if err != nil {
		s.mapDBError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"unread_count": n})
}

type markEventsReadReq struct {
	UpToCursor   *time.Time `json:"up_to_cursor"`
	UpToCursorID *uuid.UUID `json:"up_to_cursor_id"`
}

// MarkEventsRead implements genclient.ServerInterface (POST /me/events/read).
// Omitted body/cursor = mark everything read. Idempotent.
func (s *Server) MarkEventsRead(w http.ResponseWriter, r *http.Request) {
	subj, ok := clientSubject(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}
	var req markEventsReadReq
	decodeOptionalJSON(r, &req)
	marked, err := s.pg.Queries.MarkEventsRead(r.Context(), sqlc.MarkEventsReadParams{
		UserID: subj, Cursor: req.UpToCursor, CursorID: req.UpToCursorID,
	})
	if err != nil {
		s.mapDBError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"marked": marked})
}
