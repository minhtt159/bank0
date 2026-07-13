package api

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/minhtt159/bank0/internal/db"
	sqlc "github.com/minhtt159/bank0/internal/db/sqlc"
)

// transferStatus reads a transfer's current lifecycle state straight from the DB.
func transferStatus(t *testing.T, pg *db.Postgres, id string) string {
	t.Helper()
	var st string
	if err := pg.Pool.QueryRow(context.Background(),
		`SELECT status::text FROM transfers WHERE id = $1`, id).Scan(&st); err != nil {
		t.Fatalf("read transfer status: %v", err)
	}
	return st
}

// screeningRequestID returns the pending screening_hold admin_actions id for a
// transfer parked under_review (the id the approve/reject endpoints act on).
func screeningRequestID(t *testing.T, pg *db.Postgres, transferID string) string {
	t.Helper()
	var id string
	if err := pg.Pool.QueryRow(context.Background(),
		`SELECT id FROM admin_actions
		  WHERE action = 'screening_hold' AND target_id = $1 AND approved_by IS NULL`,
		transferID).Scan(&id); err != nil {
		t.Fatalf("find screening request: %v", err)
	}
	return id
}

// mkWatchlistedMule creates a customer whose registered full_name is distinctive and
// matched by a fresh active watchlist entry, so any payment to it is screened. The
// distinctive tag keeps the pattern from matching the other fixtures' "T" full_name.
func mkWatchlistedMule(t *testing.T, pg *db.Postgres) (uuid.UUID, uuid.UUID) {
	t.Helper()
	muleID, _ := mkUser(t, pg, sqlc.UserRoleCustomer)
	muleAcct := mkAcct(t, pg, muleID, 0)
	tag := "Sanctioned Mule " + uhex(8)
	if _, err := pg.Pool.Exec(context.Background(),
		`UPDATE users SET full_name = $1 WHERE id = $2`, tag, muleID); err != nil {
		t.Fatalf("set mule name: %v", err)
	}
	if _, err := pg.Queries.CreateWatchlistEntry(context.Background(), sqlc.CreateWatchlistEntryParams{
		Pattern: "%" + tag + "%", Reason: "test sanctions hit", Active: true,
	}); err != nil {
		t.Fatalf("create watchlist entry: %v", err)
	}
	return muleID, muleAcct
}

// Rec 25: a payment to a watchlisted party is parked under_review and surfaces in the
// console screening queue. Approve releases & posts it; reject refuses & cancels it.
// Both reuse the maker-checker endpoints (widened to screening_hold rows).
func TestHTTPConsoleScreening(t *testing.T) {
	ts, pg := newTestServer(t)
	t.Cleanup(func() { _, _ = pg.Pool.Exec(context.Background(), `TRUNCATE watchlist_entries`) })

	payerID, payerName := mkUser(t, pg, sqlc.UserRoleCustomer)
	payerAcct := mkAcct(t, pg, payerID, 100_000)
	_, muleAcct := mkWatchlistedMule(t, pg)

	_, adminName := mkUser(t, pg, sqlc.UserRoleAdmin)
	_, opName := mkUser(t, pg, sqlc.UserRoleOperator)
	admin := login(t, ts, adminName, "pw")
	op := login(t, ts, opName, "pw")
	payerTok := clientToken(t, ts, payerName, "pw")

	// (1) a payment to the watchlisted mule is held under_review, not posted.
	tid := postTransfer(t, ts, payerTok, payerAcct, muleAcct, 1000)
	if st := transferStatus(t, pg, tid); st != "under_review" {
		t.Fatalf("screened transfer status = %q, want under_review", st)
	}

	// it appears in the screening queue with the release/refuse actions.
	if r := get(t, admin, ts.URL+"/console/screenings/results", nil); r.StatusCode != 200 {
		t.Fatalf("screenings results = %d, want 200", r.StatusCode)
	} else if b := body(t, r); !strings.Contains(b, "Release &amp; post") || !strings.Contains(b, "Refuse &amp; cancel") {
		t.Errorf("screening queue missing actions; body=%.300s", b)
	}

	// RBAC: canApprove is admin-only — an operator cannot release.
	reqID := screeningRequestID(t, pg, tid)
	if code, _ := sessForm(t, op, ts.URL+"/console/approvals/"+reqID+"/approve", url.Values{}); code != 403 {
		t.Errorf("operator release = %d, want 403", code)
	}

	// (2) admin releases -> the transfer posts and the mule is credited.
	if code, b := sessForm(t, admin, ts.URL+"/console/approvals/"+reqID+"/approve", url.Values{}); code != 200 || !strings.Contains(b, "Approved and posted") {
		t.Fatalf("admin release = %d; body=%.200s", code, b)
	}
	if st := transferStatus(t, pg, tid); st != "posted" {
		t.Errorf("after release status = %q, want posted", st)
	}
	if bal, _ := acctBalance(t, pg, muleAcct); bal != 1000 {
		t.Errorf("mule balance after release = %d, want 1000", bal)
	}
	// released row leaves the queue.
	if b := body(t, get(t, admin, ts.URL+"/console/screenings/results", nil)); strings.Contains(b, reqID) {
		t.Errorf("released row still in screening queue")
	}

	// (3) a second screened payment, this time refused -> canceled, funds released.
	tid2 := postTransfer(t, ts, payerTok, payerAcct, muleAcct, 2000)
	if st := transferStatus(t, pg, tid2); st != "under_review" {
		t.Fatalf("second screened transfer status = %q, want under_review", st)
	}
	reqID2 := screeningRequestID(t, pg, tid2)
	if code, b := sessForm(t, admin, ts.URL+"/console/approvals/"+reqID2+"/reject", url.Values{}); code != 200 || !strings.Contains(b, "Request rejected") {
		t.Fatalf("admin refuse = %d; body=%.200s", code, b)
	}
	if st := transferStatus(t, pg, tid2); st != "canceled" {
		t.Errorf("after refuse status = %q, want canceled", st)
	}
	// the mule was never credited by the refused payment.
	if bal, _ := acctBalance(t, pg, muleAcct); bal != 1000 {
		t.Errorf("mule balance after refuse = %d, want 1000 (unchanged)", bal)
	}
}
