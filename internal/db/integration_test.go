package db

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/minhtt159/bank0/internal/config"
	"github.com/minhtt159/bank0/internal/iban"
	sqlc "github.com/minhtt159/bank0/internal/db/sqlc"
	"github.com/minhtt159/bank0/internal/migrate"
)

// These exercise the real PL/pgSQL — where bank0's correctness actually lives.
// They run only when TEST_DATABASE_DSN points at a disposable Postgres; otherwise
// they skip, so `go test ./...` stays green without Docker.
//
//	createdb bank0_test
//	TEST_DATABASE_DSN='postgres://admin:admin@localhost:5432/bank0_test?sslmode=disable' go test ./internal/db/

var testDSN = os.Getenv("TEST_DATABASE_DSN")

func TestMain(m *testing.M) {
	if testDSN != "" {
		if err := migrate.Up(testDSN); err != nil {
			fmt.Fprintln(os.Stderr, "migrate test db:", err)
			os.Exit(1)
		}
	}
	os.Exit(m.Run())
}

func newTestPG(t *testing.T) *Postgres {
	t.Helper()
	if testDSN == "" {
		t.Skip("set TEST_DATABASE_DSN to run DB integration tests")
	}
	pg, err := NewPostgres(config.DatabaseConfig{
		DSN: testDSN, MaxOpenConns: 5, MaxIdleConns: 2, ConnTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pg.Close)
	return pg
}

// --- fixtures (unique entities so tests don't collide / need truncation) ---

func uniqHex(n int) string {
	return strings.ToUpper(strings.ReplaceAll(uuid.NewString(), "-", ""))[:n]
}

func mkCustomer(t *testing.T, pg *Postgres) uuid.UUID {
	t.Helper()
	id, err := pg.Queries.CreateUser(context.Background(), sqlc.CreateUserParams{
		Username: "u" + uniqHex(16), Password: "pw", FullName: "Test User", Role: sqlc.UserRoleCustomer,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return id
}

func mkAccount(t *testing.T, pg *Postgres, owner uuid.UUID) uuid.UUID {
	t.Helper()
	ibanStr, err := iban.Generate("SE")
	if err != nil {
		t.Fatalf("gen iban: %v", err)
	}
	id, err := pg.Queries.CreateAccount(context.Background(), sqlc.CreateAccountParams{
		UserID: owner, Iban: ibanStr, Pin: "1234", TransferLimitMinor: 100_000_000,
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	return id
}

func fund(t *testing.T, pg *Postgres, acct uuid.UUID, minor int64) {
	t.Helper()
	if _, err := pg.Queries.Deposit(context.Background(), sqlc.DepositParams{
		IdempotencyKey: uuid.NewString(), AccountID: acct, AmountMinor: minor, Description: "test fund",
	}); err != nil {
		t.Fatalf("fund: %v", err)
	}
}

func balance(t *testing.T, pg *Postgres, acct uuid.UUID) (ledger, available int64) {
	t.Helper()
	a, err := pg.Queries.GetAccount(context.Background(), acct)
	if err != nil {
		t.Fatalf("get account: %v", err)
	}
	return a.BalanceMinor, a.AvailableMinor
}

func reconcileClean(t *testing.T, pg *Postgres) {
	t.Helper()
	issues, err := pg.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(issues) != 0 {
		t.Fatalf("ledger invariants broken: %+v", issues)
	}
}

// --- tests ---------------------------------------------------------------

func TestTransferDoubleEntryAndReconcile(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	a := mkAccount(t, pg, mkCustomer(t, pg))
	b := mkAccount(t, pg, mkCustomer(t, pg))
	fund(t, pg, a, 10_000)

	res, err := pg.Transfer(ctx, uuid.NewString(), a, b, 3_000, "rent", sqlc.TransferKindTransfer)
	if err != nil {
		t.Fatalf("transfer: %v", err)
	}
	if res.Status != sqlc.TransferStatusPosted || res.WasReplay {
		t.Fatalf("expected posted, not replay; got %+v", res)
	}

	if lb, _ := balance(t, pg, a); lb != 7_000 {
		t.Errorf("debit balance = %d, want 7000", lb)
	}
	if lb, _ := balance(t, pg, b); lb != 3_000 {
		t.Errorf("credit balance = %d, want 3000", lb)
	}
	reconcileClean(t, pg) // global double-entry invariant still holds
}

func TestIdempotentTransferReplay(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	a := mkAccount(t, pg, mkCustomer(t, pg))
	b := mkAccount(t, pg, mkCustomer(t, pg))
	fund(t, pg, a, 10_000)

	key := uuid.NewString()
	r1, err := pg.Transfer(ctx, key, a, b, 2_500, "x", sqlc.TransferKindTransfer)
	if err != nil {
		t.Fatalf("transfer1: %v", err)
	}
	r2, err := pg.Transfer(ctx, key, a, b, 2_500, "x", sqlc.TransferKindTransfer)
	if err != nil {
		t.Fatalf("transfer2 (replay): %v", err)
	}
	if !r2.WasReplay || r2.TransferID != r1.TransferID {
		t.Errorf("replay should return original; r1=%v r2=%v", r1, r2)
	}
	if lb, _ := balance(t, pg, a); lb != 7_500 { // debited once, not twice
		t.Errorf("balance after replay = %d, want 7500 (single debit)", lb)
	}
}

func TestInsufficientFundsRejected(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	a := mkAccount(t, pg, mkCustomer(t, pg))
	b := mkAccount(t, pg, mkCustomer(t, pg))
	fund(t, pg, a, 1_000)

	if _, err := pg.Transfer(ctx, uuid.NewString(), a, b, 5_000, "too much", sqlc.TransferKindTransfer); err == nil {
		t.Fatal("expected insufficient-funds error")
	}
	if lb, _ := balance(t, pg, a); lb != 1_000 {
		t.Errorf("failed transfer must not move money; balance=%d", lb)
	}
	reconcileClean(t, pg)
}

func TestHoldsReduceAvailableAndCancelReleases(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	a := mkAccount(t, pg, mkCustomer(t, pg))
	b := mkAccount(t, pg, mkCustomer(t, pg))
	fund(t, pg, a, 10_000)

	req, err := pg.RequestTransfer(ctx, uuid.NewString(), a, b, 4_000, "pending", sqlc.TransferKindTransfer)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if req.Status != sqlc.TransferStatusPending {
		t.Fatalf("want pending; got %s", req.Status)
	}
	led, avail := balance(t, pg, a)
	if led != 10_000 || avail != 6_000 { // hold reserves 4000 of available, ledger unchanged
		t.Errorf("ledger=%d available=%d, want 10000/6000", led, avail)
	}

	if _, err := pg.Queries.CancelTransfer(ctx, sqlc.CancelTransferParams{ID: req.TransferID, Reason: "test"}); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if led, avail := balance(t, pg, a); led != 10_000 || avail != 10_000 {
		t.Errorf("after cancel ledger=%d available=%d, want 10000/10000", led, avail)
	}
}

func TestPostTransferLifecycle(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	a := mkAccount(t, pg, mkCustomer(t, pg))
	b := mkAccount(t, pg, mkCustomer(t, pg))
	fund(t, pg, a, 10_000)

	req, _ := pg.RequestTransfer(ctx, uuid.NewString(), a, b, 1_000, "x", sqlc.TransferKindTransfer)
	if st, err := pg.Queries.PostTransfer(ctx, req.TransferID); err != nil || st != sqlc.TransferStatusPosted {
		t.Fatalf("post: %v st=%s", err, st)
	}
	// canceling an already-posted transfer must fail (illegal state transition)
	if _, err := pg.Queries.CancelTransfer(ctx, sqlc.CancelTransferParams{ID: req.TransferID, Reason: "late"}); err == nil {
		t.Error("cancel after post must be rejected")
	}
	reconcileClean(t, pg)
}

func TestReverseAppendsInverseAndRebalances(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	a := mkAccount(t, pg, mkCustomer(t, pg))
	b := mkAccount(t, pg, mkCustomer(t, pg))
	fund(t, pg, a, 10_000)

	res, _ := pg.Transfer(ctx, uuid.NewString(), a, b, 4_000, "oops", sqlc.TransferKindTransfer)
	if _, err := pg.Queries.ReverseTransfer(ctx, sqlc.ReverseTransferParams{
		ID: res.TransferID, IdempotencyKey: uuid.NewString(), Reason: "mistake",
	}); err != nil {
		t.Fatalf("reverse: %v", err)
	}
	// money returns; original is marked reversed (never edited)
	if lb, _ := balance(t, pg, a); lb != 10_000 {
		t.Errorf("debit restored = %d, want 10000", lb)
	}
	if lb, _ := balance(t, pg, b); lb != 0 {
		t.Errorf("credit reversed = %d, want 0", lb)
	}
	got, _ := pg.Queries.GetTransfer(ctx, res.TransferID)
	if got.Status != sqlc.TransferStatusReversed {
		t.Errorf("original status = %s, want reversed", got.Status)
	}
	reconcileClean(t, pg)
}

func TestMakerCheckerFourEyes(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	maker := mkCustomer(t, pg) // any two distinct user ids
	checker := mkCustomer(t, pg)
	acct := mkAccount(t, pg, mkCustomer(t, pg))

	var tid uuid.UUID
	if err := pg.Pool.QueryRow(ctx,
		`SELECT request_deposit($1,$2,$3,$4)`, uuid.NewString(), acct, int64(2_000_000), "big credit").Scan(&tid); err != nil {
		t.Fatalf("request_deposit: %v", err)
	}
	var reqID uuid.UUID
	if err := pg.Pool.QueryRow(ctx,
		`SELECT create_approval_request($1,$2,$3)`, maker, tid, []byte(`{}`)).Scan(&reqID); err != nil {
		t.Fatalf("create_approval_request: %v", err)
	}

	// maker approving their own request must be refused (4-eyes)
	if _, err := pg.Pool.Exec(ctx, `SELECT approve_request($1,$2)`, reqID, maker); err == nil {
		t.Fatal("self-approval must be rejected")
	}
	// the pending deposit must still be pending (not posted by the failed attempt)
	if got, _ := pg.Queries.GetTransfer(ctx, tid); got.Status != sqlc.TransferStatusPending {
		t.Fatalf("transfer should still be pending; got %s", got.Status)
	}
	// a different admin approves -> posts
	if _, err := pg.Pool.Exec(ctx, `SELECT approve_request($1,$2)`, reqID, checker); err != nil {
		t.Fatalf("approve by checker: %v", err)
	}
	if got, _ := pg.Queries.GetTransfer(ctx, tid); got.Status != sqlc.TransferStatusPosted {
		t.Errorf("after approval status = %s, want posted", got.Status)
	}
	// double-approve must fail (already handled)
	if _, err := pg.Pool.Exec(ctx, `SELECT approve_request($1,$2)`, reqID, checker); err == nil {
		t.Error("re-approving a handled request must fail")
	}
}

// TestPaginationKeysetCoversTies is the regression test for the cursor bug:
// many rows inserted in ONE transaction share an identical requested_at, so a
// timestamp-only cursor would skip them. The composite (requested_at, id) cursor
// must page through ALL of them with no skips or duplicates.
func TestPaginationKeysetCoversTies(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	a := mkAccount(t, pg, mkCustomer(t, pg))
	b := mkAccount(t, pg, mkCustomer(t, pg))
	fund(t, pg, a, 1_000_000)

	// High-entropy marker (no shared prefix) so we page only our own rows and
	// remain robust to data left in the test DB by previous runs.
	tag := "kt" + uniqHex(12)
	const n = 25
	created := make(map[uuid.UUID]bool, n)
	tx, err := pg.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		var id uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT transfer_id FROM transfer($1,$2,$3,$4,$5,'transfer')`,
			uuid.NewString(), a, b, int64(100), fmt.Sprintf("%s #%d", tag, i)).Scan(&id); err != nil {
			tx.Rollback(ctx)
			t.Fatalf("seed transfer %d: %v", i, err)
		}
		created[id] = true
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	q := tag
	seen := map[uuid.UUID]bool{}
	var curTS *time.Time
	var curID *uuid.UUID
	for page := 0; page < 100; page++ {
		rows, err := pg.Queries.SearchTransfers(ctx, sqlc.SearchTransfersParams{
			Q: &q, Cursor: curTS, CursorID: curID, PageLimit: 7,
		})
		if err != nil {
			t.Fatalf("search page %d: %v", page, err)
		}
		if len(rows) == 0 {
			break
		}
		for _, r := range rows {
			if seen[r.ID] {
				t.Fatalf("duplicate row across pages: %s", r.ID)
			}
			seen[r.ID] = true
		}
		last := rows[len(rows)-1]
		curTS, curID = &last.RequestedAt, &last.ID
		if len(rows) < 7 {
			break
		}
	}
	// Every created row shares one requested_at; all must be reachable across
	// pages. A timestamp-only cursor would skip the ties and miss some.
	for id := range created {
		if !seen[id] {
			t.Fatalf("keyset pagination SKIPPED a tied row: %s", id)
		}
	}
}

func TestWithdrawAndMakerCheckerWithdrawal(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	acct := mkAccount(t, pg, mkCustomer(t, pg))
	fund(t, pg, acct, 10_000)

	// Small withdrawal auto-posts (money leaves to external_clearing).
	if _, err := pg.Queries.Withdraw(ctx, sqlc.WithdrawParams{
		IdempotencyKey: uuid.NewString(), AccountID: acct, AmountMinor: 3_000, Description: "wd",
	}); err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	if lb, _ := balance(t, pg, acct); lb != 7_000 {
		t.Errorf("balance after withdraw = %d, want 7000", lb)
	}
	reconcileClean(t, pg)

	// Large withdrawal staged PENDING: the hold reserves funds, ledger unchanged.
	tid, err := pg.Queries.RequestWithdrawal(ctx, sqlc.RequestWithdrawalParams{
		IdempotencyKey: uuid.NewString(), AccountID: acct, AmountMinor: 5_000, Description: "big wd",
	})
	if err != nil {
		t.Fatalf("request_withdrawal: %v", err)
	}
	if led, avail := balance(t, pg, acct); led != 7_000 || avail != 2_000 {
		t.Errorf("pending withdrawal: ledger=%d available=%d, want 7000/2000", led, avail)
	}

	// A different admin approves -> the withdrawal posts; balance drops.
	maker := mkCustomer(t, pg)
	checker := mkCustomer(t, pg)
	var reqID uuid.UUID
	if err := pg.Pool.QueryRow(ctx, `SELECT create_approval_request($1,$2,$3)`, maker, tid, []byte(`{}`)).Scan(&reqID); err != nil {
		t.Fatalf("create_approval_request: %v", err)
	}
	if _, err := pg.Pool.Exec(ctx, `SELECT approve_request($1,$2)`, reqID, checker); err != nil {
		t.Fatalf("approve withdrawal: %v", err)
	}
	if lb, _ := balance(t, pg, acct); lb != 2_000 {
		t.Errorf("balance after approved withdrawal = %d, want 2000", lb)
	}
	reconcileClean(t, pg)
}
