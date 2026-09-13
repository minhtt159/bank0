package db

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// bcrypt cost policy (00020_bcrypt_cost_12.sql, issue #121). Two properties:
// every function that writes a password hash writes cost 12, and the two login
// functions re-cost an older hash on a successful verification. The second is
// the one worth a test — it is the only path that can upgrade a hash, it runs on
// every login, and a silent break leaves the whole user table at cost 10 with
// nothing to notice it.

// costOf reads the cost field out of a stored bcrypt hash ("$2a$12$...").
func costOf(t *testing.T, pg *Postgres, id uuid.UUID) int {
	t.Helper()
	var cost int
	if err := pg.Pool.QueryRow(context.Background(),
		`SELECT split_part(password_hash, '$', 3)::INT FROM users WHERE id = $1`, id,
	).Scan(&cost); err != nil {
		t.Fatalf("read bcrypt cost: %v", err)
	}
	return cost
}

// downgrade plants a cost-10 hash, standing in for a row written before 00020.
func downgrade(t *testing.T, pg *Postgres, id uuid.UUID, password string) {
	t.Helper()
	if _, err := pg.Pool.Exec(context.Background(),
		`UPDATE users SET password_hash = crypt($2, gen_salt('bf', 10)) WHERE id = $1`, id, password,
	); err != nil {
		t.Fatalf("plant cost-10 hash: %v", err)
	}
	if got := costOf(t, pg, id); got != 10 {
		t.Fatalf("planted hash cost = %d, want 10", got)
	}
}

func TestNewPasswordHashesUseCost12(t *testing.T) {
	pg := newTestPG(t)
	_, id := mkStaff(t, pg, "operator", "pw-cost-probe")

	if got := costOf(t, pg, id); got != 12 {
		t.Fatalf("create_user wrote cost %d, want 12", got)
	}
}

func TestStaffLoginRehashesStaleCost(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()

	const password = "pw-rehash-staff"
	username, id := mkStaff(t, pg, "operator", password)
	downgrade(t, pg, id, password)

	login := func() error {
		var uid uuid.UUID
		var uname, role string
		return pg.Pool.QueryRow(ctx,
			`SELECT user_id, username, role FROM create_staff_session($1,$2,$3,$4,$5,$6)`,
			username, password, uniqHex(32), 900, "ua", "1.2.3.4",
		).Scan(&uid, &uname, &role)
	}

	if err := login(); err != nil {
		t.Fatalf("login with the cost-10 hash: %v", err)
	}
	if got := costOf(t, pg, id); got != 12 {
		t.Fatalf("after login cost = %d, want 12 (hash was not re-costed)", got)
	}
	// The upgraded hash must still verify the same password — a rehash that
	// stored the wrong thing would lock the user out on their next visit.
	if err := login(); err != nil {
		t.Fatalf("login again after the rehash: %v", err)
	}
	if got := costOf(t, pg, id); got != 12 {
		t.Fatalf("second login cost = %d, want a stable 12", got)
	}
}

func TestClientLoginRehashesStaleCost(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()

	cust := mkCustomer(t, pg)
	username := usernameOf(t, pg, cust)
	downgrade(t, pg, cust, "pw")

	rows := func(pw string) int {
		var n int
		if err := pg.Pool.QueryRow(ctx,
			`SELECT count(*) FROM check_user_credentials($1,$2)`, username, pw,
		).Scan(&n); err != nil {
			t.Fatalf("check_user_credentials: %v", err)
		}
		return n
	}

	// A failed attempt must NOT rehash: re-costing on a wrong password would let
	// an unauthenticated caller spend bcrypt work per guess.
	if got := rows("wrong-password"); got != 0 {
		t.Fatalf("bad password returned %d rows, want 0", got)
	}
	if got := costOf(t, pg, cust); got != 10 {
		t.Fatalf("failed login changed cost to %d, want it left at 10", got)
	}

	if got := rows("pw"); got != 1 {
		t.Fatalf("good password returned %d rows, want 1", got)
	}
	if got := costOf(t, pg, cust); got != 12 {
		t.Fatalf("after login cost = %d, want 12 (hash was not re-costed)", got)
	}
	if got := rows("pw"); got != 1 {
		t.Fatalf("login after the rehash returned %d rows, want 1", got)
	}
}
