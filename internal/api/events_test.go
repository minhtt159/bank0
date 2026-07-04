package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	sqlc "github.com/minhtt159/bank0/internal/db/sqlc"
)

// HTTP notification feed: scoping, filters, badge, mark-read.

func TestHTTPEventsFeed(t *testing.T) {
	ts, pg := newTestServer(t)
	aliceID, aliceName := mkUser(t, pg, sqlc.UserRoleCustomer)
	aliceAcct := mkAcct(t, pg, aliceID, 10_000)
	bobID, bobName := mkUser(t, pg, sqlc.UserRoleCustomer)
	bobAcct := mkAcct(t, pg, bobID, 0)

	aliceTok := bearerFor(t, ts.URL, aliceName, "pw")
	aliceHdr := map[string]string{"Authorization": "Bearer " + aliceTok}

	// Transfer alice -> bob.
	r := postJSON(t, ts.URL+"/transfers",
		map[string]string{"Authorization": "Bearer " + aliceTok, "Idempotency-Key": uuid.NewString()},
		map[string]any{"debit_account": aliceAcct.String(), "credit_account": bobAcct.String(), "amount_minor": 2_500})
	if r.StatusCode != http.StatusOK {
		t.Fatalf("transfer = %d", r.StatusCode)
	}

	// Alice's feed: transfer.posted + payment.incoming (her funding deposit) + device.new (her login).
	feed := get(t, http.DefaultClient, ts.URL+"/me/events", aliceHdr)
	if feed.StatusCode != http.StatusOK {
		t.Fatalf("feed = %d", feed.StatusCode)
	}
	var items []struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	decodeBody(t, feed, &items)
	types := map[string]int{}
	for _, it := range items {
		types[it.Type]++
	}
	if types["transfer.posted"] != 1 || types["payment.incoming"] != 1 || types["device.new"] != 1 {
		t.Errorf("alice feed types = %v, want one each of posted/incoming/device", types)
	}
	// data is a JSON object, not base64.
	if len(items) > 0 && !strings.HasPrefix(strings.TrimSpace(string(items[0].Data)), "{") {
		t.Errorf("data not an object: %s", items[0].Data)
	}

	// Bob sees his incoming payment; he never sees alice's device event.
	bobTok := bearerFor(t, ts.URL, bobName, "pw")
	bobHdr := map[string]string{"Authorization": "Bearer " + bobTok}
	bf := get(t, http.DefaultClient, ts.URL+"/me/events?type=payment.incoming", bobHdr)
	if bf.StatusCode != http.StatusOK {
		t.Fatalf("bob feed = %d", bf.StatusCode)
	}
	var bobItems []struct {
		Type string `json:"type"`
	}
	decodeBody(t, bf, &bobItems)
	if len(bobItems) != 1 || bobItems[0].Type != "payment.incoming" {
		t.Errorf("bob filtered feed = %+v, want exactly one payment.incoming", bobItems)
	}

	// Filters: bogus type -> 400. Fresh user -> [] not null.
	if r := get(t, http.DefaultClient, ts.URL+"/me/events?type=bogus", aliceHdr); r.StatusCode != http.StatusBadRequest {
		t.Errorf("bogus type = %d, want 400", r.StatusCode)
	}
	freshID, freshName := mkUser(t, pg, sqlc.UserRoleCustomer)
	_ = freshID
	freshTok := bearerFor(t, ts.URL, freshName, "pw")
	fr := get(t, http.DefaultClient, ts.URL+"/me/events?type=payment.incoming",
		map[string]string{"Authorization": "Bearer " + freshTok})
	if b := body(t, fr); strings.TrimSpace(b) != "[]" {
		t.Errorf("fresh filtered feed = %q, want []", b)
	}

	// Badge + mark-read: unread drops to 0, idempotent.
	ur := get(t, http.DefaultClient, ts.URL+"/me/events/unread", aliceHdr)
	var unread struct {
		UnreadCount int `json:"unread_count"`
	}
	decodeBody(t, ur, &unread)
	if unread.UnreadCount < 3 {
		t.Errorf("unread = %d, want >= 3", unread.UnreadCount)
	}
	mr := postJSON(t, ts.URL+"/me/events/read", aliceHdr, nil)
	if mr.StatusCode != http.StatusOK {
		t.Fatalf("mark read = %d", mr.StatusCode)
	}
	var marked struct {
		Marked int `json:"marked"`
	}
	decodeBody(t, mr, &marked)
	if marked.Marked != unread.UnreadCount {
		t.Errorf("marked = %d, want %d", marked.Marked, unread.UnreadCount)
	}
	ur2 := get(t, http.DefaultClient, ts.URL+"/me/events/unread", aliceHdr)
	decodeBody(t, ur2, &unread)
	if unread.UnreadCount != 0 {
		t.Errorf("unread after mark = %d, want 0", unread.UnreadCount)
	}

	// unread_only now returns [].
	uo := get(t, http.DefaultClient, ts.URL+"/me/events?unread_only=true", aliceHdr)
	if b := body(t, uo); strings.TrimSpace(b) != "[]" {
		t.Errorf("unread_only after mark = %q, want []", b)
	}

	// No bearer -> 401.
	if r := get(t, newClient(), ts.URL+"/me/events", nil); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("anon feed = %d, want 401", r.StatusCode)
	}
}
