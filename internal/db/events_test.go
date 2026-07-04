package db

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// Notification feed (00008): emissions ride the source transitions' txns.

func TestPostTransferEmitsBothPartyEvents(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	alice := mkCustomer(t, pg)
	bob := mkCustomer(t, pg)
	a := mkAccount(t, pg, alice)
	b := mkAccount(t, pg, bob)
	fund(t, pg, a, 10_000)

	tid := postedTransfer(t, pg, a, b)

	var typ, data string
	if err := pg.Pool.QueryRow(ctx,
		`SELECT type, data::text FROM events WHERE user_id = $1 AND related_transfer_id = $2`,
		alice, tid).Scan(&typ, &data); err != nil {
		t.Fatalf("payer event: %v", err)
	}
	if typ != "transfer.posted" || !strings.Contains(data, `"amount_minor": 3000`) {
		t.Errorf("payer event = %s %s, want transfer.posted with amount 3000", typ, data)
	}
	if err := pg.Pool.QueryRow(ctx,
		`SELECT type FROM events WHERE user_id = $1 AND related_transfer_id = $2`,
		bob, tid).Scan(&typ); err != nil {
		t.Fatalf("payee event: %v", err)
	}
	if typ != "payment.incoming" {
		t.Errorf("payee event = %s, want payment.incoming", typ)
	}

	// A deposit (system -> customer) notifies only the customer: no NULL-user rows
	// exist at all (system sides emit nothing).
	var badRows int
	if err := pg.Pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE user_id IS NULL`).Scan(&badRows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if badRows != 0 {
		t.Errorf("events with NULL user = %d, want 0", badRows)
	}
	// fund() already deposited into a: the deposit produced a payment.incoming for alice.
	var n int
	if err := pg.Pool.QueryRow(ctx,
		`SELECT count(*) FROM events e JOIN transfers tr ON tr.id = e.related_transfer_id
		  WHERE e.user_id = $1 AND e.type = 'payment.incoming' AND tr.kind = 'deposit'`, alice).Scan(&n); err != nil {
		t.Fatalf("deposit event: %v", err)
	}
	if n != 1 {
		t.Errorf("deposit payment.incoming = %d, want 1", n)
	}
}

func TestEventEmissionIdempotentOnReplay(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	a := mkAccount(t, pg, mkCustomer(t, pg))
	b := mkAccount(t, pg, mkCustomer(t, pg))
	fund(t, pg, a, 10_000)

	key := uuid.NewString()
	for i := 0; i < 2; i++ { // second call = idempotent replay
		if _, err := pg.Pool.Exec(ctx,
			`SELECT transfer($1,$2,$3,$4,$5,'transfer')`, key, a, b, int64(2_000), "replayed"); err != nil {
			t.Fatalf("transfer #%d: %v", i+1, err)
		}
	}
	var n int
	if err := pg.Pool.QueryRow(ctx,
		`SELECT count(*) FROM events e JOIN transfers tr ON tr.id = e.related_transfer_id
		  WHERE tr.idempotency_key = $1`, key).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 { // exactly one per party, not doubled
		t.Errorf("events after replay = %d, want 2 (payer + payee)", n)
	}
}

func TestIssueRefreshTokenEmitsDeviceEventOnce(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	u := mkCustomer(t, pg)

	var family uuid.UUID
	if err := pg.Pool.QueryRow(ctx,
		`SELECT issue_refresh_token($1, $2, 3600, 'test-agent', '203.0.113.9', 'phone')`,
		u, "rt-"+uuid.NewString()).Scan(&family); err != nil {
		t.Fatalf("issue: %v", err)
	}
	var n int
	if err := pg.Pool.QueryRow(ctx,
		`SELECT count(*) FROM events WHERE user_id = $1 AND type = 'device.new'
		  AND data->>'family_id' = $2::text`, u, family).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("device.new for family = %d, want 1", n)
	}
	// A second login = a second family = a second event (families are distinct devices).
	if err := pg.Pool.QueryRow(ctx,
		`SELECT issue_refresh_token($1, $2, 3600, 'test-agent', '203.0.113.9', 'phone')`,
		u, "rt-"+uuid.NewString()).Scan(&family); err != nil {
		t.Fatalf("issue #2: %v", err)
	}
	if err := pg.Pool.QueryRow(ctx,
		`SELECT count(*) FROM events WHERE user_id = $1 AND type = 'device.new'`, u).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Errorf("device.new events = %d, want 2 (one per family)", n)
	}
}

func TestResolveDisputeEmitsPerStatusChange(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	ownerA := mkCustomer(t, pg)
	resolver := mkCustomer(t, pg)
	a := mkAccount(t, pg, ownerA)
	b := mkAccount(t, pg, mkCustomer(t, pg))
	fund(t, pg, a, 10_000)
	tid := postedTransfer(t, pg, a, b)

	var did uuid.UUID
	if err := pg.Pool.QueryRow(ctx,
		`SELECT raise_dispute($1,$2,'unrecognised','not me')`, tid, ownerA).Scan(&did); err != nil {
		t.Fatalf("raise: %v", err)
	}
	for _, st := range []string{"under_review", "resolved"} {
		if _, err := pg.Pool.Exec(ctx,
			`SELECT resolve_dispute($1,$2,$3::dispute_status,'')`, did, resolver, st); err != nil {
			t.Fatalf("resolve %s: %v", st, err)
		}
	}
	var n int
	if err := pg.Pool.QueryRow(ctx,
		`SELECT count(*) FROM events WHERE user_id = $1 AND type = 'dispute.updated'
		  AND data->>'dispute_id' = $2::text`, ownerA, did).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 { // one per status change, NOT collapsed
		t.Errorf("dispute.updated events = %d, want 2", n)
	}
}

func TestEventsAppendOnlyAndMarkRead(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	a := mkAccount(t, pg, mkCustomer(t, pg))
	b := mkAccount(t, pg, mkCustomer(t, pg))
	fund(t, pg, a, 10_000)
	postedTransfer(t, pg, a, b)

	var owner uuid.UUID
	if err := pg.Pool.QueryRow(ctx, `SELECT user_id FROM accounts WHERE id = $1`, a).Scan(&owner); err != nil {
		t.Fatalf("owner: %v", err)
	}

	// Append-only: DELETE and non-read_at UPDATE both blocked (23001).
	if _, err := pg.Pool.Exec(ctx, `DELETE FROM events WHERE user_id = $1`, owner); sqlstate(err) != "23001" {
		t.Errorf("DELETE SQLSTATE = %q, want 23001", sqlstate(err))
	}
	if _, err := pg.Pool.Exec(ctx,
		`UPDATE events SET title = 'rewritten' WHERE user_id = $1`, owner); sqlstate(err) != "23001" {
		t.Errorf("UPDATE title SQLSTATE = %q, want 23001", sqlstate(err))
	}

	// mark_events_read (read_at is the one mutable column): all → count drops to 0.
	var unread int
	_ = pg.Pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE user_id = $1 AND read_at IS NULL`, owner).Scan(&unread)
	if unread == 0 {
		t.Fatal("expected unread events from the funded transfer")
	}
	var marked int
	if err := pg.Pool.QueryRow(ctx, `SELECT mark_events_read($1)`, owner).Scan(&marked); err != nil {
		t.Fatalf("mark: %v", err)
	}
	if marked != unread {
		t.Errorf("marked = %d, want %d", marked, unread)
	}
	_ = pg.Pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE user_id = $1 AND read_at IS NULL`, owner).Scan(&unread)
	if unread != 0 {
		t.Errorf("unread after mark-all = %d, want 0", unread)
	}
	// Idempotent re-mark.
	if err := pg.Pool.QueryRow(ctx, `SELECT mark_events_read($1)`, owner).Scan(&marked); err != nil || marked != 0 {
		t.Errorf("re-mark = (%d, %v), want (0, nil)", marked, err)
	}
}
