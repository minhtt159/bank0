package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/minhtt159/bank0/internal/db"
	sqlc "github.com/minhtt159/bank0/internal/db/sqlc"
)

// resetDisputes clears the disputes table so global (non-user-scoped) assertions —
// e.g. the admin "?status=resolved is empty" queue check — are isolated from rows
// left by earlier tests in the shared DB. Nothing FK-references disputes.
func resetDisputes(t *testing.T, pg *db.Postgres) {
	t.Helper()
	if _, err := pg.Pool.Exec(context.Background(), `TRUNCATE disputes`); err != nil {
		t.Fatalf("truncate disputes: %v", err)
	}
}

func postTransfer(t *testing.T, ts *httptest.Server, token string, debit, credit uuid.UUID, amount int64) string {
	t.Helper()
	b := `{"debit_account":"` + debit.String() + `","credit_account":"` + credit.String() +
		`","amount_minor":` + strconv.FormatInt(amount, 10) + `}`
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/transfers", strings.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Idempotency-Key", uuid.NewString())
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("transfer: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("setup transfer = %d, want 200", resp.StatusCode)
	}
	var out struct {
		TransferID string `json:"transfer_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out.TransferID
}

func doDispute(t *testing.T, ts *httptest.Server, token, transferID, jsonBody string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/transfers/"+transferID+"/dispute", strings.NewReader(jsonBody))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("dispute: %v", err)
	}
	return resp.StatusCode, body(t, resp)
}

func clientGet(t *testing.T, ts *httptest.Server, token, path string) (int, string) {
	t.Helper()
	hdr := map[string]string{}
	if token != "" {
		hdr["Authorization"] = "Bearer " + token
	}
	resp := get(t, newClient(), ts.URL+path, hdr)
	return resp.StatusCode, body(t, resp)
}

func sessPost(t *testing.T, c *http.Client, url, jsonBody string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func disputeID(t *testing.T, b string) string {
	t.Helper()
	var d struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(b), &d); err != nil {
		t.Fatalf("decode dispute id: %v; body=%s", err, b)
	}
	return d.ID
}

// gjson reads a top-level string field from a JSON object body.
func gjson(t *testing.T, b, key string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(b), &m); err != nil {
		t.Fatalf("decode json: %v; body=%s", err, b)
	}
	s, _ := m[key].(string)
	return s
}

// Client surface: raise (party-only, one-open, category-validated), list/get scoped
// to the raiser, empty list is []. See spec-disputes.md.
func TestHTTPDisputesClient(t *testing.T) {
	ts, pg := newTestServer(t)
	resetDisputes(t, pg)
	aliceID, aliceName := mkUser(t, pg, sqlc.UserRoleCustomer)
	aliceAcct := mkAcct(t, pg, aliceID, 100_000)
	bobID, bobName := mkUser(t, pg, sqlc.UserRoleCustomer)
	bobAcct := mkAcct(t, pg, bobID, 0)
	_, charlieName := mkUser(t, pg, sqlc.UserRoleCustomer)
	aliceTok := clientToken(t, ts, aliceName, "pw")
	bobTok := clientToken(t, ts, bobName, "pw")
	charlieTok := clientToken(t, ts, charlieName, "pw")

	tid := postTransfer(t, ts, aliceTok, aliceAcct, bobAcct, 1000) // posted alice -> bob

	// no bearer -> 401
	if code, _ := doDispute(t, ts, "", tid, `{"category":"fraud"}`); code != 401 {
		t.Errorf("no-bearer raise = %d, want 401", code)
	}
	// not a party -> 404 (existence hidden)
	if code, _ := doDispute(t, ts, charlieTok, tid, `{"category":"fraud"}`); code != 404 {
		t.Errorf("non-party raise = %d, want 404", code)
	}
	// invalid category -> 422
	if code, _ := doDispute(t, ts, aliceTok, tid, `{"category":"bogus"}`); code != 422 {
		t.Errorf("bad category = %d, want 422", code)
	}
	// happy: alice (debit owner) raises -> 201 open
	code, b := doDispute(t, ts, aliceTok, tid, `{"category":"fraud","reason":"not me"}`)
	if code != 201 {
		t.Fatalf("raise = %d, want 201; body=%s", code, b)
	}
	var d struct {
		ID, Status, Category, TransferID string
	}
	d.Status = gjson(t, b, "status")
	d.Category = gjson(t, b, "category")
	d.TransferID = gjson(t, b, "transfer_id")
	d.ID = gjson(t, b, "id")
	if d.Status != "open" || d.Category != "fraud" || d.TransferID != tid {
		t.Errorf("dispute = %+v", d)
	}
	aliceDisputeID := d.ID

	// duplicate open -> 409
	if code, _ := doDispute(t, ts, aliceTok, tid, `{"category":"fraud"}`); code != 409 {
		t.Errorf("dup raise = %d, want 409", code)
	}
	// bob (credit owner) may also raise -> 201 (a separate row per raiser)
	if code, _ := doDispute(t, ts, bobTok, tid, `{"category":"unrecognised"}`); code != 201 {
		t.Errorf("bob raise = %d, want 201", code)
	}

	// alice lists only hers (1); empty list (charlie) is [] not null
	if lc, lb := clientGet(t, ts, aliceTok, "/disputes"); lc != 200 || strings.Count(lb, `"id"`) != 1 {
		t.Errorf("alice list = %d, count=%d; body=%s", lc, strings.Count(lb, `"id"`), lb)
	}
	if ec, eb := clientGet(t, ts, charlieTok, "/disputes"); ec != 200 || strings.TrimSpace(eb) != "[]" {
		t.Errorf("empty list = %d %q, want 200 []", ec, eb)
	}
	// get own -> 200; charlie getting alice's dispute -> 404 (never revealed)
	if gc, _ := clientGet(t, ts, aliceTok, "/disputes/"+aliceDisputeID); gc != 200 {
		t.Errorf("get own = %d, want 200", gc)
	}
	if fc, _ := clientGet(t, ts, charlieTok, "/disputes/"+aliceDisputeID); fc != 404 {
		t.Errorf("get foreign = %d, want 404", fc)
	}

	// non-disputable (pending) transfer -> 422
	pend, err := pg.RequestTransfer(context.Background(), uuid.NewString(), aliceAcct, bobAcct, 500, "pending", sqlc.TransferKindTransfer)
	if err != nil {
		t.Fatalf("request pending transfer: %v", err)
	}
	if code, _ := doDispute(t, ts, aliceTok, pend.TransferID.String(), `{"category":"fraud"}`); code != 422 {
		t.Errorf("dispute pending transfer = %d, want 422", code)
	}
}

// Admin surface: triage queue (+ status filter), resolve state machine (illegal -> 409),
// and the client then sees the resolved status + note.
func TestHTTPDisputesAdmin(t *testing.T) {
	ts, pg := newTestServer(t)
	resetDisputes(t, pg)
	aliceID, aliceName := mkUser(t, pg, sqlc.UserRoleCustomer)
	aliceAcct := mkAcct(t, pg, aliceID, 100_000)
	bobID, _ := mkUser(t, pg, sqlc.UserRoleCustomer)
	bobAcct := mkAcct(t, pg, bobID, 0)
	_, adminName := mkUser(t, pg, sqlc.UserRoleAdmin)
	aliceTok := clientToken(t, ts, aliceName, "pw")

	tid := postTransfer(t, ts, aliceTok, aliceAcct, bobAcct, 1000)
	code, b := doDispute(t, ts, aliceTok, tid, `{"category":"fraud","reason":"x"}`)
	if code != 201 {
		t.Fatalf("raise = %d; body=%s", code, b)
	}
	did := disputeID(t, b)

	// no session -> 401
	if r := get(t, newClient(), ts.URL+"/admin/disputes", nil); r.StatusCode != 401 {
		t.Errorf("no-session queue = %d, want 401", r.StatusCode)
	}

	admin := login(t, ts, adminName, "pw")
	// queue ?status=open lists it; ?status=resolved is empty
	if r := get(t, admin, ts.URL+"/admin/disputes?status=open", nil); r.StatusCode != 200 {
		t.Errorf("queue ?open = %d, want 200", r.StatusCode)
	} else if qb := body(t, r); !strings.Contains(qb, did) || !strings.Contains(qb, aliceName) {
		t.Errorf("queue ?open missing dispute/raiser; body=%.250s", qb)
	}
	if r := get(t, admin, ts.URL+"/admin/disputes?status=resolved", nil); r.StatusCode != 200 {
		t.Errorf("queue ?resolved = %d", r.StatusCode)
	} else if rb := body(t, r); strings.TrimSpace(rb) != "[]" {
		t.Errorf("queue ?resolved = %q, want []", rb)
	}

	// resolve -> 200; repeat on a terminal dispute -> 409; invalid status -> 422
	if sc := sessPost(t, admin, ts.URL+"/admin/disputes/"+did+"/resolve", `{"status":"resolved","resolution_note":"refunded"}`); sc != 200 {
		t.Fatalf("resolve = %d, want 200", sc)
	}
	if sc := sessPost(t, admin, ts.URL+"/admin/disputes/"+did+"/resolve", `{"status":"rejected"}`); sc != 409 {
		t.Errorf("re-resolve terminal = %d, want 409", sc)
	}
	if sc := sessPost(t, admin, ts.URL+"/admin/disputes/"+did+"/resolve", `{"status":"open"}`); sc != 422 {
		t.Errorf("invalid status = %d, want 422", sc)
	}

	// the client now sees resolved + the operator note
	if gc, gb := clientGet(t, ts, aliceTok, "/disputes/"+did); gc != 200 ||
		gjson(t, gb, "status") != "resolved" || !strings.Contains(gb, "refunded") {
		t.Errorf("client view after resolve = %d; body=%s", gc, gb)
	}
}
