package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/minhtt159/bank0/internal/db"
	sqlc "github.com/minhtt159/bank0/internal/db/sqlc"
)

// createXfer POSTs /transfers as the bearer caller with a fresh idempotency key and
// returns the response.
func createXfer(t *testing.T, tsURL, tok string, debit, credit uuid.UUID, amount int64) *http.Response {
	t.Helper()
	return postJSON(t, tsURL+"/transfers", map[string]string{
		"Authorization":   "Bearer " + tok,
		"Idempotency-Key": uuid.NewString(),
	}, map[string]any{
		"debit_account": debit.String(), "credit_account": credit.String(), "amount_minor": amount,
	})
}

// backdateAck inserts an acknowledged warning_acks row for THIS exact payment, aged
// so it satisfies assert_warning_ack's cooling-off window. The table is append-only
// but permits INSERT with an explicit created_at.
func backdateAck(t *testing.T, pg *db.Postgres, user, debit uuid.UUID, credIban string, amount int64, ageSecs int) {
	t.Helper()
	if _, err := pg.Pool.Exec(context.Background(),
		`INSERT INTO warning_acks (user_id, category, reason_code, acknowledged,
		     debit_account_id, counterparty_iban, amount_minor, device, created_at)
		 VALUES ($1, 'risk_warning', '', TRUE, $2, $3, $4, 'web', now() - make_interval(secs => $5))`,
		user, debit, credIban, amount, ageSecs); err != nil {
		t.Fatalf("insert ack: %v", err)
	}
}

// TestHTTPCreateTransferAckRequired: a required-ack warning rule makes POST /transfers
// return 409 ack_required; once a correctly-aged acknowledgement exists, the retry posts.
func TestHTTPCreateTransferAckRequired(t *testing.T) {
	ts, pg := newTestServer(t)
	clearGates(t, pg)
	aliceID, aliceName := mkUser(t, pg, sqlc.UserRoleCustomer)
	aliceAcct := mkAcct(t, pg, aliceID, 1_000_000)
	bobID, _ := mkUser(t, pg, sqlc.UserRoleCustomer)
	bobAcct := mkAcct(t, pg, bobID, 0)
	bobIban := creditIban(t, pg, bobAcct)

	const cool, amt = 60, int64(5_000)
	addWarningRule(t, pg, "first_payment_to_payee", "", "risk_warning", "warn", true, cool, 100)

	tok := clientToken(t, ts, aliceName, "pw")

	// No ack yet -> 409 ack_required.
	r := createXfer(t, ts.URL, tok, aliceAcct, bobAcct, amt)
	if r.StatusCode != http.StatusConflict {
		t.Fatalf("missing-ack transfer = %d, want 409: %s", r.StatusCode, body(t, r))
	}
	if b := body(t, r); !strings.Contains(b, `"error":"ack_required"`) {
		t.Errorf("missing-ack body = %s, want ack_required", b)
	}

	// Record a correctly-aged acknowledgement for this exact payment, retry -> posts.
	backdateAck(t, pg, aliceID, aliceAcct, bobIban, amt, cool+5)
	r = createXfer(t, ts.URL, tok, aliceAcct, bobAcct, amt)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("acked transfer = %d, want 200: %s", r.StatusCode, body(t, r))
	}
	if b := body(t, r); !strings.Contains(b, `"status":"posted"`) {
		t.Errorf("acked transfer body = %s, want posted", b)
	}
}

// TestHTTPCreateTransferBlocked: a block rule makes POST /transfers return
// 422 payment_blocked and moves no money.
func TestHTTPCreateTransferBlocked(t *testing.T) {
	ts, pg := newTestServer(t)
	clearGates(t, pg)
	aliceID, aliceName := mkUser(t, pg, sqlc.UserRoleCustomer)
	aliceAcct := mkAcct(t, pg, aliceID, 1_000_000)
	bobID, _ := mkUser(t, pg, sqlc.UserRoleCustomer)
	bobAcct := mkAcct(t, pg, bobID, 0)

	addWarningRule(t, pg, "first_payment_to_payee", "", "risk_warning", "block", false, 0, 100)

	tok := clientToken(t, ts, aliceName, "pw")
	r := createXfer(t, ts.URL, tok, aliceAcct, bobAcct, 5_000)
	if r.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("blocked transfer = %d, want 422: %s", r.StatusCode, body(t, r))
	}
	if b := body(t, r); !strings.Contains(b, `"error":"payment_blocked"`) {
		t.Errorf("blocked body = %s, want payment_blocked", b)
	}
}

// TestHTTPCreateTransferReviewHeld: a review rule parks the payment as held; GET
// /transfers/{id} exposes the hold_reason/hold_expires_at fields; the owner confirms
// to post it, a non-owner is 404.
func TestHTTPCreateTransferReviewHeld(t *testing.T) {
	ts, pg := newTestServer(t)
	clearGates(t, pg)
	aliceID, aliceName := mkUser(t, pg, sqlc.UserRoleCustomer)
	aliceAcct := mkAcct(t, pg, aliceID, 1_000_000)
	bobID, bobName := mkUser(t, pg, sqlc.UserRoleCustomer)
	bobAcct := mkAcct(t, pg, bobID, 0)

	addWarningRule(t, pg, "first_payment_to_payee", "", "risk_warning", "review", false, 0, 100)

	tok := clientToken(t, ts, aliceName, "pw")
	r := createXfer(t, ts.URL, tok, aliceAcct, bobAcct, 5_000)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("review transfer = %d, want 200: %s", r.StatusCode, body(t, r))
	}
	var res struct {
		TransferID string `json:"transfer_id"`
		Status     string `json:"status"`
	}
	decodeBody(t, r, &res)
	if res.Status != "held" {
		t.Fatalf("review transfer status = %q, want held", res.Status)
	}

	// GET /transfers/{id} exposes the hold fields.
	auth := map[string]string{"Authorization": "Bearer " + tok}
	gr := get(t, http.DefaultClient, ts.URL+"/transfers/"+res.TransferID, auth)
	gb := body(t, gr)
	if !strings.Contains(gb, `"status":"held"`) || !strings.Contains(gb, `"hold_reason"`) ||
		!strings.Contains(gb, `"hold_expires_at"`) {
		t.Errorf("GET held transfer missing hold fields: %s", gb)
	}

	// A non-owner cannot confirm -> 404 (hides existence).
	bobTok := clientToken(t, ts, bobName, "pw")
	if cr := postJSON(t, ts.URL+"/transfers/"+res.TransferID+"/confirm",
		map[string]string{"Authorization": "Bearer " + bobTok}, nil); cr.StatusCode != http.StatusNotFound {
		t.Errorf("non-owner confirm = %d, want 404", cr.StatusCode)
	}

	// The owner confirms -> posted; confirming again is idempotent.
	cr := postJSON(t, ts.URL+"/transfers/"+res.TransferID+"/confirm", auth, nil)
	if cr.StatusCode != http.StatusOK {
		t.Fatalf("owner confirm = %d, want 200: %s", cr.StatusCode, body(t, cr))
	}
	if b := body(t, cr); !strings.Contains(b, `"status":"posted"`) {
		t.Errorf("confirm body = %s, want posted", b)
	}
	if cr := postJSON(t, ts.URL+"/transfers/"+res.TransferID+"/confirm", auth, nil); cr.StatusCode != http.StatusOK {
		t.Errorf("idempotent re-confirm = %d, want 200", cr.StatusCode)
	}
}

// TestHTTPWatchlistScreeningUnderReview: a watchlist name match parks the payment as
// under_review; the customer can neither confirm nor cancel it (both 409).
func TestHTTPWatchlistScreeningUnderReview(t *testing.T) {
	ts, pg := newTestServer(t)
	clearGates(t, pg)
	aliceID, aliceName := mkUser(t, pg, sqlc.UserRoleCustomer)
	aliceAcct := mkAcct(t, pg, aliceID, 1_000_000)
	bobID, _ := mkUser(t, pg, sqlc.UserRoleCustomer)
	bobAcct := mkAcct(t, pg, bobID, 0)
	// Give the payee a distinctive name and screen it.
	if _, err := pg.Pool.Exec(context.Background(),
		`UPDATE users SET full_name = 'Ivan Sanctioned' WHERE id = $1`, bobID); err != nil {
		t.Fatalf("set payee name: %v", err)
	}
	addWatchlistEntry(t, pg, "%Sanctioned%", "test AML hit")

	tok := clientToken(t, ts, aliceName, "pw")
	auth := map[string]string{"Authorization": "Bearer " + tok}

	r := createXfer(t, ts.URL, tok, aliceAcct, bobAcct, 5_000)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("screened transfer = %d, want 200: %s", r.StatusCode, body(t, r))
	}
	var res struct {
		TransferID string `json:"transfer_id"`
		Status     string `json:"status"`
	}
	decodeBody(t, r, &res)
	if res.Status != "under_review" {
		t.Fatalf("screened transfer status = %q, want under_review", res.Status)
	}

	// The customer cannot confirm an under_review transfer (operator-only) -> 409.
	if cr := postJSON(t, ts.URL+"/transfers/"+res.TransferID+"/confirm", auth, nil); cr.StatusCode != http.StatusConflict {
		t.Errorf("confirm under_review = %d, want 409: %s", cr.StatusCode, body(t, cr))
	}
	// Nor cancel it -> 409.
	if cr := postJSON(t, ts.URL+"/transfers/"+res.TransferID+"/cancel", auth, nil); cr.StatusCode != http.StatusConflict {
		t.Errorf("cancel under_review = %d, want 409: %s", cr.StatusCode, body(t, cr))
	}
}

// TestHTTPReverseTwiceSameReversal: reversing an already-reversed transfer over HTTP
// with a DIFFERENT idempotency key returns the SAME reversal id (Rec 4 idempotence).
func TestHTTPReverseTwiceSameReversal(t *testing.T) {
	ts, pg := newTestServer(t)
	clearGates(t, pg)
	aliceID, aliceName := mkUser(t, pg, sqlc.UserRoleCustomer)
	aliceAcct := mkAcct(t, pg, aliceID, 1_000_000)
	bobID, _ := mkUser(t, pg, sqlc.UserRoleCustomer)
	bobAcct := mkAcct(t, pg, bobID, 0)
	_, adminName := mkUser(t, pg, sqlc.UserRoleAdmin)

	// A plain (no-rule) transfer posts.
	tok := clientToken(t, ts, aliceName, "pw")
	r := createXfer(t, ts.URL, tok, aliceAcct, bobAcct, 5_000)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("seed transfer = %d, want 200: %s", r.StatusCode, body(t, r))
	}
	var res struct {
		TransferID string `json:"transfer_id"`
		Status     string `json:"status"`
	}
	decodeBody(t, r, &res)
	if res.Status != "posted" {
		t.Fatalf("seed transfer status = %q, want posted", res.Status)
	}

	admin := login(t, ts, adminName, "pw")
	reverse := func(key string) string {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/transfers/"+res.TransferID+"/reverse",
			strings.NewReader(`{"reason":"test"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", key)
		resp, err := admin.Do(req)
		if err != nil {
			t.Fatalf("reverse: %v", err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("reverse = %d, want 200: %s", resp.StatusCode, body(t, resp))
		}
		var out struct {
			ReversalID string `json:"reversal_id"`
		}
		decodeBody(t, resp, &out)
		return out.ReversalID
	}

	first := reverse(uuid.NewString())
	second := reverse(uuid.NewString()) // different key, already-reversed transfer
	if first == "" || first != second {
		t.Errorf("second reverse returned %q, want the existing reversal id %q", second, first)
	}
}
