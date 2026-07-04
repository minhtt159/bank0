package api

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"
	sqlc "github.com/minhtt159/bank0/internal/db/sqlc"
)

// HTTP: PSR claim decide/recall (admin) + recipient-risk fields on resolve.

func TestHTTPDisputeDecideAndRecall(t *testing.T) {
	ts, pg := newTestServer(t)
	victimID, victimName := mkUser(t, pg, sqlc.UserRoleCustomer)
	victimAcct := mkAcct(t, pg, victimID, 100_000)
	otherID, _ := mkUser(t, pg, sqlc.UserRoleCustomer)
	otherAcct := mkAcct(t, pg, otherID, 0)
	_, adminName := mkUser(t, pg, sqlc.UserRoleAdmin)

	// Victim pays, then raises a scam-tagged dispute.
	tok := bearerFor(t, ts.URL, victimName, "pw")
	hdr := map[string]string{"Authorization": "Bearer " + tok, "Idempotency-Key": uuid.NewString()}
	r := postJSON(t, ts.URL+"/transfers", hdr, map[string]any{
		"debit_account": victimAcct.String(), "credit_account": otherAcct.String(), "amount_minor": 50_000})
	if r.StatusCode != http.StatusOK {
		t.Fatalf("transfer = %d", r.StatusCode)
	}
	var tr struct {
		TransferID string `json:"transfer_id"`
	}
	decodeBody(t, r, &tr)

	r = postJSON(t, ts.URL+"/transfers/"+tr.TransferID+"/dispute",
		map[string]string{"Authorization": "Bearer " + tok},
		map[string]any{"category": "fraud", "reason": "scammed", "scam_type": "impersonation"})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("dispute = %d: %s", r.StatusCode, body(t, r))
	}
	var d struct {
		ID       string `json:"id"`
		ScamType string `json:"scam_type"`
		SlaDueAt string `json:"sla_due_at"`
		Decision string `json:"decision"`
	}
	decodeBody(t, r, &d)
	if d.ScamType != "impersonation" || d.SlaDueAt == "" || d.Decision != "pending" {
		t.Fatalf("claim fields = %+v", d)
	}

	admin := login(t, ts, adminName, "pw")

	// Recall request, then decide (partial 20000 -> 10000 after the €100 excess).
	rr, err := admin.Post(ts.URL+"/admin/disputes/"+d.ID+"/recall", "application/json",
		strings.NewReader(`{"status":"requested","reason":"FRAD"}`))
	if err != nil || rr.StatusCode != http.StatusOK {
		t.Fatalf("recall = %v/%d", err, rr.StatusCode)
	}
	dr, err := admin.Post(ts.URL+"/admin/disputes/"+d.ID+"/decide", "application/json",
		strings.NewReader(`{"decision":"partially_reimbursed","reimbursed_amount_minor":20000,"note":"partial"}`))
	if err != nil || dr.StatusCode != http.StatusOK {
		t.Fatalf("decide = %v/%d: %s", err, dr.StatusCode, body(t, dr))
	}
	var out struct {
		PayoutMinor int64 `json:"payout_minor"`
	}
	decodeBody(t, dr, &out)
	if out.PayoutMinor != 10_000 {
		t.Errorf("payout = %d, want 10000", out.PayoutMinor)
	}

	// Second decide -> 409. Customer sees the outcome + an event.
	dr2, _ := admin.Post(ts.URL+"/admin/disputes/"+d.ID+"/decide", "application/json",
		strings.NewReader(`{"decision":"declined"}`))
	if dr2.StatusCode != http.StatusConflict {
		t.Errorf("double decide = %d, want 409", dr2.StatusCode)
	}
	g := get(t, http.DefaultClient, ts.URL+"/disputes/"+d.ID, map[string]string{"Authorization": "Bearer " + tok})
	if b := body(t, g); !strings.Contains(b, `"decision":"partially_reimbursed"`) || !strings.Contains(b, `"recall_status":"requested"`) {
		t.Errorf("customer dispute view missing claim fields: %s", b)
	}
}

func TestHTTPResolveRecipientRisk(t *testing.T) {
	ts, pg := newTestServer(t)
	callerID, callerName := mkUser(t, pg, sqlc.UserRoleCustomer)
	_ = mkAcct(t, pg, callerID, 1_000)
	muleID, _ := mkUser(t, pg, sqlc.UserRoleCustomer)
	muleAcct := mkAcct(t, pg, muleID, 0)
	scenario := "http-risk-" + uhex(8)
	if _, err := pg.Pool.Exec(t.Context(),
		`INSERT INTO guided_scenarios (name, target_account_id, target_user_id) VALUES ($1, $2, $3)`,
		scenario, muleAcct, callerID); err != nil {
		t.Fatalf("flag mule: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pg.Pool.Exec(context.Background(), `DELETE FROM guided_scenarios WHERE name = $1`, scenario)
	})
	var muleIban string
	_ = pg.Pool.QueryRow(t.Context(), `SELECT iban FROM accounts WHERE id=$1`, muleAcct).Scan(&muleIban)

	tok := bearerFor(t, ts.URL, callerName, "pw")
	r := get(t, http.DefaultClient,
		ts.URL+"/beneficiaries/resolve?iban="+url.QueryEscape(muleIban),
		map[string]string{"Authorization": "Bearer " + tok})
	if r.StatusCode != http.StatusOK {
		t.Fatalf("resolve = %d", r.StatusCode)
	}
	b := body(t, r)
	for _, want := range []string{`"recipient_risk":"high"`, `"mule_suspected":true`, `"mule_flagged"`, `"is_first_payment_to_payee":true`} {
		if !strings.Contains(b, want) {
			t.Errorf("resolve body missing %s: %s", want, b)
		}
	}
}
