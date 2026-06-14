package api

import (
	"context"
	"html"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/minhtt159/bank0/internal/db"
	sqlc "github.com/minhtt159/bank0/internal/db/sqlc"
	"github.com/minhtt159/bank0/internal/iban"
)

// The operator console list pages (Users / Accounts / Approvals / Disputes) were
// hard-capped at DefaultPageLimit with no Prev/Next: rows past the first page were
// unreachable. These tests prove the keyset pager now pages through ALL rows with
// no skips and no duplicates — the HTTP analogue of the DB-level
// TestPaginationKeysetCoversTies (which guards transfers).
//
// To exercise the (created_at, id) tiebreak each batch is inserted inside ONE
// transaction, so every seeded row shares an identical created_at. A timestamp-only
// cursor would skip the ties at a page boundary.

// nextLink pulls the Next button's results URL (the one carrying a cursor) out of a
// rendered pager fragment. templ HTML-escapes the query string (& -> &amp;), so we
// unescape before handing the URL back to the client.
func nextLink(htmlStr, resultsPath string) string {
	re := regexp.MustCompile(`hx-get="(` + regexp.QuoteMeta(resultsPath) + `\?[^"]*cursor=[^"]*)"`)
	m := re.FindStringSubmatch(htmlStr)
	if m == nil {
		return ""
	}
	return html.UnescapeString(m[1])
}

// rowIDs extracts the per-row entity UUIDs from a results fragment by reading the
// id out of each row's drill-down hx-get link (e.g. /console/users/<uuid>).
func rowIDs(htmlStr, linkPrefix string) []uuid.UUID {
	re := regexp.MustCompile(regexp.QuoteMeta(linkPrefix) + `([0-9a-fA-F-]{36})`)
	var out []uuid.UUID
	for _, m := range re.FindAllStringSubmatch(htmlStr, -1) {
		if id, err := uuid.Parse(m[1]); err == nil {
			out = append(out, id)
		}
	}
	return out
}

// pageThrough walks the pager from firstURL to the last page, following each Next
// link. It fails on a duplicate row id across pages and returns every id seen.
// linkPrefix identifies the per-row drill-down link the id is read from.
func pageThrough(t *testing.T, ts *httptest.Server, c *http.Client, firstURL, resultsPath, linkPrefix string) map[uuid.UUID]bool {
	t.Helper()
	seen := map[uuid.UUID]bool{}
	url := firstURL
	sawNext := false
	for page := 0; page < 100; page++ {
		htmlStr := body(t, get(t, c, url, nil))
		for _, id := range rowIDs(htmlStr, linkPrefix) {
			if seen[id] {
				t.Fatalf("duplicate row across pages: %s (page %d)", id, page)
			}
			seen[id] = true
		}
		next := nextLink(htmlStr, resultsPath)
		if next == "" {
			break
		}
		sawNext = true
		url = ts.URL + next
	}
	if !sawNext {
		t.Fatalf("a >1-page list exposed no Next link: %s", firstURL)
	}
	return seen
}

func TestHTTPConsoleUsersPaginate(t *testing.T) {
	ts, pg := newTestServer(t)
	_, adminName := mkUser(t, pg, sqlc.UserRoleAdmin)
	admin := login(t, ts, adminName, "pw")

	// 26 (> DefaultPageLimit=25) users sharing a full_name tag, inserted in one
	// transaction so they share created_at. We page by ?q=<tag> (the search FILTER)
	// so we only ever see our own rows — robust to data left by other tests.
	tag := uniqTag("pgu")
	want := seedTaggedUsers(t, pg, tag, 26)

	// First page returns exactly DefaultPageLimit rows and a Next link (the bug:
	// no Next link existed, so row 26 was unreachable).
	first := body(t, get(t, admin, ts.URL+"/console/users/results?q="+tag, nil))
	if got := len(rowIDs(first, "/console/users/")); got != 25 {
		t.Fatalf("first users page = %d rows, want 25 (DefaultPageLimit)", got)
	}
	if nextLink(first, "/console/users/results") == "" {
		t.Fatal("first users page must offer a Next link when more rows exist")
	}

	seen := pageThrough(t, ts, admin, ts.URL+"/console/users/results?q="+tag,
		"/console/users/results", "/console/users/")
	assertCovers(t, "users", want, seen)
}

func TestHTTPConsoleAccountsPaginate(t *testing.T) {
	ts, pg := newTestServer(t)
	_, adminName := mkUser(t, pg, sqlc.UserRoleAdmin)
	admin := login(t, ts, adminName, "pw")

	// 26 accounts owned by one tagged user, inserted in one transaction (shared
	// created_at). Account rows drill into /console/users/<owner> (one repeated
	// owner id), so we cannot dedupe by row link — instead we assert the two pages
	// carry DISJOINT account IBANs and together cover all 26.
	tag := uniqTag("pga")
	ibans := seedTaggedAccounts(t, pg, tag, 26)

	first := body(t, get(t, admin, ts.URL+"/console/accounts/results?q="+tag, nil))
	next := nextLink(first, "/console/accounts/results")
	if next == "" {
		t.Fatal("first accounts page must offer a Next link when more rows exist")
	}
	second := body(t, get(t, admin, ts.URL+next, nil))

	p1 := ibansIn(first, ibans)
	p2 := ibansIn(second, ibans)
	if len(p1) != 25 {
		t.Fatalf("first accounts page = %d tagged rows, want 25", len(p1))
	}
	for ib := range p2 {
		if p1[ib] {
			t.Fatalf("account IBAN %s appeared on both pages (keyset overlap)", ib)
		}
	}
	union := map[string]bool{}
	for ib := range p1 {
		union[ib] = true
	}
	for ib := range p2 {
		union[ib] = true
	}
	if len(union) != 26 {
		t.Fatalf("two accounts pages covered %d of 26 distinct IBANs", len(union))
	}
}

// --- helpers --------------------------------------------------------------

func uniqTag(prefix string) string {
	return prefix + strings.ToUpper(strings.ReplaceAll(uuid.NewString(), "-", ""))[:12]
}

func seedTaggedUsers(t *testing.T, pg *db.Postgres, tag string, n int) map[uuid.UUID]bool {
	t.Helper()
	ctx := context.Background()
	tx, err := pg.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	ids := make(map[uuid.UUID]bool, n)
	for i := 0; i < n; i++ {
		var id uuid.UUID
		// full_name carries the tag so ?q=<tag> matches via the ILIKE filter.
		uname := tag + uhex(8) + "x"
		if err := tx.QueryRow(ctx,
			`INSERT INTO users (username, password_hash, full_name, role)
			 VALUES ($1, 'x', $2, 'customer') RETURNING id`,
			uname, tag+" person",
		).Scan(&id); err != nil {
			t.Fatalf("seed user %d: %v", i, err)
		}
		ids[id] = true
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return ids
}

// seedTaggedAccounts creates n accounts under one tagged owner (so ?q=<tag> matches
// via owner full_name) in a single transaction (shared created_at). Returns the set
// of IBANs created.
func seedTaggedAccounts(t *testing.T, pg *db.Postgres, tag string, n int) map[string]bool {
	t.Helper()
	ctx := context.Background()
	owner, err := pg.Queries.CreateUser(ctx, sqlc.CreateUserParams{
		Username: tag + "owner", Password: "pw", FullName: tag + " owner", Role: sqlc.UserRoleCustomer,
	})
	if err != nil {
		t.Fatalf("seed owner: %v", err)
	}
	tx, err := pg.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	ibans := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		// A real, checksum-valid IBAN — the accounts table has a
		// CHECK (iban_is_valid(iban)) backstop (migration 00022).
		ib, err := iban.Generate("NL")
		if err != nil {
			t.Fatalf("gen iban %d: %v", i, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO accounts (user_id, kind, iban, pin_hash, status)
			 VALUES ($1, 'customer', $2, 'x', 'active')`,
			owner, ib,
		); err != nil {
			t.Fatalf("seed account %d: %v", i, err)
		}
		ibans[ib] = true
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return ibans
}

// ibansIn returns the subset of want IBANs that appear in the rendered fragment.
func ibansIn(htmlStr string, want map[string]bool) map[string]bool {
	got := map[string]bool{}
	for ib := range want {
		if strings.Contains(htmlStr, ib) {
			got[ib] = true
		}
	}
	return got
}

func assertCovers(t *testing.T, what string, want, seen map[uuid.UUID]bool) {
	t.Helper()
	for id := range want {
		if !seen[id] {
			t.Fatalf("%s pagination SKIPPED a row: %s (seen %d of %d)", what, id, len(seen), len(want))
		}
	}
}
