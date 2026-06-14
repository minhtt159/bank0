// Command seedgen emits a self-contained, idempotent demo/pentest seed
// (db/seed_demo.sql): many customers, accounts, and transactions for demos, pentests,
// and handing fraudbank realistic data. It is a dev tool, not part of the app build.
//
//	go run ./db/seedgen                 # ~400 users / ~1000 accounts / ~500 txns, random each run
//	go run ./db/seedgen -seed 42        # reproducible
//	go run ./db/seedgen -users 200 -accounts 500 -txns 300
//
// Account IBANs are checksum-VALID (ISO 13616 / MOD-97 via internal/iban): real ones
// drawn from the vendored db/seedgen/ibans pool first, then freshly GENERATED valid
// IBANs across many countries for the remainder — so the count isn't capped by the
// vendored set and the new accounts.iban CHECK (migration 00022) is satisfied.
//
// For speed at scale the generated SQL computes the bcrypt password/PIN hashes ONCE
// and bulk-inserts; balances + activity go through the real deposit()/transfer()/
// request_transfer()/cancel_transfer()/reverse_transfer() functions so the ledger
// reconciles. A few transactions are left PENDING and a few CANCELED/REVERSED.
// Idempotent (ON CONFLICT + idempotency-keyed money calls).
package main

import (
	"bufio"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/minhtt159/bank0/internal/iban"
)

var firstNames = []string{
	"Alice", "Bjorn", "Carla", "Dmitri", "Elena", "Felix", "Greta", "Hassan", "Ingrid", "Jonas",
	"Katja", "Liam", "Mira", "Niko", "Olga", "Pavel", "Quinn", "Rosa", "Stefan", "Tara",
	"Umar", "Vera", "Wouter", "Xenia", "Yusuf", "Zoe", "Anders", "Bea", "Cyril", "Dana",
	"Emil", "Farida", "Gustav", "Hanna", "Ivan", "Julia", "Karim", "Lena", "Marco", "Nadia",
	"Oscar", "Petra", "Rashid", "Sara", "Tomas", "Ulrika", "Viktor", "Wei", "Yara", "Zane",
	"Amara", "Bruno", "Chiara", "Diego", "Eva", "Fabio", "Gabriela", "Henrik", "Isabel", "Jamal",
	"Klara", "Leo", "Maya", "Noah", "Omar", "Paula", "Rafael", "Sofia", "Theo", "Uma",
	"Valentina", "Walid", "Ximena", "Yann", "Zara", "Aron", "Beatriz", "Cem", "Dilara", "Esra",
	"Finn", "Giulia", "Hugo", "Ines", "Janik", "Kasia", "Lars", "Marta", "Nils", "Oona",
	"Pedro", "Reza", "Selma", "Tobias", "Ulla", "Vlad", "Wiktor", "Yuki", "Zeynep", "Aino",
}

var lastNames = []string{
	"Andersson", "Bergstrom", "Costa", "Dubois", "Eriksson", "Fischer", "Garcia", "Hansen", "Ivanov", "Jensen",
	"Kowalski", "Larsen", "Muller", "Nielsen", "Olsen", "Petrov", "Quintero", "Rossi", "Schmidt", "Toth",
	"Ucar", "Vidal", "Weber", "Novak", "Yilmaz", "Zhang", "Bianchi", "Moreau", "Nowak", "Horvath",
	"OBrien", "Lefebvre", "Sorensen", "Kovac", "Lindqvist", "Romano", "Walsh", "Janssen", "Berg", "Haas",
	"Almeida", "Bauer", "Conti", "Dias", "Esposito", "Fontaine", "Greco", "Hoffmann", "Ibrahim", "Jovanovic",
	"Keller", "Lorenzo", "Marchetti", "Nilsson", "Ortega", "Pereira", "Ricci", "Santos", "Tremblay", " Unknown",
	"Vasquez", "Wagner", "Yildiz", "Zimmermann", "Aliyev", "Brandt", "Cabrera", "Dominguez", "Engel", "Ferrari",
	"Gallo", "Hartmann", "Ivanova", "Jonsson", "Kaur", "Lehmann", "Marin", "Nguyen", "Okafor", "Popescu",
	"Reyes", "Schulz", "Tanaka", "Ucok", "Vargas", "Wolff", "Yusuf", "Zubkov", "Adamski", "Becker",
	"Carlsson", "Demir", "Egan", "Forsberg", "Gruber", "Holm", "Iversen", "Jakobsen", "Klein", "Lund",
}

// reasonTemplates seed the ~100-string reason pool. %d slots get a random ref number.
var reasonTemplates = []string{
	"Rent", "Rent %d", "Invoice #%d", "Order %d", "Dinner", "Groceries", "Salary", "Salary %d",
	"Gift", "Refund", "Refund #%d", "Subscription", "Consulting fee", "Loan repayment", "Loan #%d",
	"Utilities", "Electricity bill", "Water bill", "Internet bill", "Phone bill", "Travel",
	"Hotel %d nights", "Flights", "Taxi", "Fuel", "Insurance premium", "Membership %d", "Tuition",
	"Childcare", "Donation", "Charity #%d", "Freelance %d", "Deposit return", "Bonus", "Commission %d",
	"Reimbursement", "Expense claim %d", "Repair", "Maintenance %d", "Rent deposit", "Catering",
	"Equipment", "Supplies order %d", "Marketing", "Advertising %d", "Software license %d",
	"Cloud hosting", "Domain renewal", "Books", "Course %d", "Coffee", "Lunch", "Birthday gift",
	"Wedding gift", "Settlement #%d", "Payout %d", "Royalties", "Dividend", "Pension", "Allowance",
}

// countries to MINT generated IBANs from (BBANs are numeric; checksum-valid for all).
var ibanCountries = []string{
	"SE", "DE", "GB", "FR", "NL", "NO", "ES", "IT", "PT", "FI", "DK", "CH", "AT", "BE", "IE",
	"PL", "CZ", "GR", "RO", "HU", "BG", "HR", "SK", "SI", "LT", "LV", "EE", "LU", "MT", "CY",
	"IS", "LI", "MC", "TR", "AE", "SA", "GE", "UA", "RS", "MK", "AL", "MD", "QA", "BH", "MU",
}

func main() {
	out := flag.String("out", "db/seed_demo.sql", "output SQL path")
	assets := flag.String("assets", "db/seedgen/ibans", "vendored IBAN dir (real IBANs preferred over generated)")
	seedFlag := flag.Int64("seed", 0, "RNG seed (0 = time-based, i.e. different each run)")
	nUsers := flag.Int("users", 400, "approx. number of customers (jittered +/-10%)")
	nAccts := flag.Int("accounts", 1000, "approx. number of accounts (jittered +/-10%)")
	nTxns := flag.Int("txns", 500, "approx. number of transactions (jittered +/-10%)")
	nReasons := flag.Int("reasons", 100, "size of the distinct transaction-reason pool")
	flag.Parse()

	seed := *seedFlag
	if seed == 0 {
		seed = time.Now().UnixNano()
	}
	rng := rand.New(rand.NewSource(seed))
	jitter := func(n int) int { return n + rng.Intn(n/5+1) - n/10 } // +/-10%

	users := jitter(*nUsers)
	accounts := jitter(*nAccts)
	txns := jitter(*nTxns)

	// Real IBAN pool (vendored) — keep only checksum-valid ones, shuffled.
	realPool := loadValidIBANs(*assets)
	rng.Shuffle(len(realPool), func(i, j int) { realPool[i], realPool[j] = realPool[j], realPool[i] })

	// --- users ---
	type user struct{ username, fullname, email, phone string }
	us := make([]user, users)
	for i := range us {
		fn := firstNames[rng.Intn(len(firstNames))]
		ln := strings.TrimSpace(lastNames[rng.Intn(len(lastNames))])
		uname := fmt.Sprintf("%s.%s%d", strings.ToLower(fn), strings.ToLower(ln), i)
		us[i] = user{uname, fn + " " + ln, uname + "@demo.example", fmt.Sprintf("+46701%06d", i)}
	}

	// --- accounts: owner, iban (real first, then generated), deposit, limit ---
	owner := make([]int, accounts)
	ibans := make([]string, accounts)
	deposit := make([]int64, accounts)
	limit := make([]int64, accounts)
	realUsed, genUsed := 0, 0
	ui := 0
	left := 1 + rng.Intn(4) // accounts for the current user
	for a := 0; a < accounts; a++ {
		if left == 0 {
			ui = (ui + 1) % users
			left = 1 + rng.Intn(4)
		}
		owner[a] = ui + 1 // 1-based
		left--
		if realUsed < len(realPool) {
			ibans[a] = realPool[realUsed]
			realUsed++
		} else {
			s, err := iban.Generate(ibanCountries[rng.Intn(len(ibanCountries))])
			if err != nil {
				fmt.Fprintln(os.Stderr, "generate iban:", err)
				os.Exit(1)
			}
			ibans[a] = s
			genUsed++
		}
		deposit[a] = int64(10000 + rng.Intn(5_000_000)) // EUR 100 .. ~50,000
		limit[a] = 1_000_000                            // EUR 10,000 transfer limit
	}

	// --- reason pool (~nReasons distinct) ---
	reasons := make([]string, 0, *nReasons)
	for len(reasons) < *nReasons {
		t := reasonTemplates[rng.Intn(len(reasonTemplates))]
		if strings.Contains(t, "%d") {
			t = fmt.Sprintf(t, 1000+rng.Intn(9000))
		}
		reasons = append(reasons, t)
	}

	// --- transactions: state-weighted (posted / pending / canceled / reversed) ---
	type txn struct {
		debit, credit int // 1-based account indices
		amount        int64
		desc, state   string
	}
	pickState := func() string {
		switch n := rng.Intn(100); {
		case n < 10:
			return "pending"
		case n < 16:
			return "canceled"
		case n < 22:
			return "reversed"
		default:
			return "posted"
		}
	}
	ts := make([]txn, 0, txns)
	for k := 0; k < txns; k++ {
		d := rng.Intn(accounts)
		c := rng.Intn(accounts)
		if d == c {
			continue
		}
		maxAmt := deposit[d]
		if maxAmt > limit[d] {
			maxAmt = limit[d]
		}
		if maxAmt < 200 {
			continue
		}
		ts = append(ts, txn{
			debit:  d + 1,
			credit: c + 1,
			amount: int64(100 + rng.Intn(int(maxAmt/3))),
			desc:   reasons[rng.Intn(len(reasons))],
			state:  pickState(),
		})
	}

	// --- emit ---
	usernames := make([]string, users)
	fullnames := make([]string, users)
	emails := make([]string, users)
	phones := make([]string, users)
	for i, u := range us {
		usernames[i], fullnames[i], emails[i], phones[i] = u.username, u.fullname, u.email, u.phone
	}
	tDebit := make([]int, len(ts))
	tCredit := make([]int, len(ts))
	tAmount := make([]int64, len(ts))
	tDesc := make([]string, len(ts))
	tState := make([]string, len(ts))
	for i, t := range ts {
		tDebit[i], tCredit[i], tAmount[i], tDesc[i], tState[i] = t.debit, t.credit, t.amount, t.desc, t.state
	}

	var b strings.Builder
	fmt.Fprintf(&b, `-- bank0 DEMO/PENTEST seed — GENERATED by db/seedgen (do not hand-edit). Idempotent.
-- %d customers, %d accounts (%d real IBANs from db/seedgen/ibans + %d generated, all
-- checksum-VALID per ISO 13616), %d transactions across %d distinct reasons.
-- A few transactions are left PENDING, and a few CANCELED / REVERSED.
--
-- Regenerate:  go run ./db/seedgen            (random each run)
--              go run ./db/seedgen -seed 42   (reproducible)
-- Apply:       task seed:demo   (or psql "$DSN" -f db/seed_demo.sql)
--
-- Demo customers share password "password"; account PINs are "1234". The bcrypt
-- hashes are computed ONCE and reused; accounts are bulk-inserted (is_default handled),
-- then funded + activity posted via the real money functions so the ledger reconciles.

DO $$
DECLARE
    v_pw    TEXT := crypt('password', gen_salt('bf', 10));
    v_pin   TEXT := crypt('1234', gen_salt('bf', 10));
    usernames TEXT[]   := %s;
    fullnames TEXT[]   := %s;
    emails    TEXT[]   := %s;
    phones    TEXT[]   := %s;
    a_iban    TEXT[]   := %s;
    a_owner   INT[]    := %s;
    a_deposit BIGINT[] := %s;
    a_limit   BIGINT[] := %s;
    t_debit   INT[]    := %s;
    t_credit  INT[]    := %s;
    t_amount  BIGINT[] := %s;
    t_desc    TEXT[]   := %s;
    t_state   TEXT[]   := %s;
    uids UUID[] := '{}';
    aids UUID[] := '{}';
    v_uid UUID;
    v_aid UUID;
    v_tid UUID;
    v_default BOOLEAN;
    i INT;
BEGIN
    FOR i IN 1 .. array_length(usernames, 1) LOOP
        INSERT INTO users (username, password_hash, full_name, email, phone_number, role)
        VALUES (usernames[i]::citext, v_pw, fullnames[i], emails[i]::citext, phones[i], 'customer')
        ON CONFLICT (username) DO NOTHING;
        SELECT id INTO v_uid FROM users WHERE username = usernames[i]::citext;
        uids[i] := v_uid;
    END LOOP;

    FOR i IN 1 .. array_length(a_iban, 1) LOOP
        v_uid := uids[a_owner[i]];
        v_default := NOT EXISTS (SELECT 1 FROM accounts WHERE user_id = v_uid AND is_default);
        INSERT INTO accounts (user_id, kind, iban, pin_hash, transfer_limit_minor, is_default, currency, status)
        VALUES (v_uid, 'customer', a_iban[i], v_pin, a_limit[i], v_default, 'EUR', 'active')
        ON CONFLICT (iban) DO NOTHING;
        SELECT id INTO v_aid FROM accounts WHERE iban = a_iban[i];
        aids[i] := v_aid;
        PERFORM deposit('seed-dep-' || a_iban[i], v_aid, a_deposit[i], 'Opening deposit');
    END LOOP;

    -- activity: most posted; some pending; some canceled / reversed. Best-effort
    -- (a transfer that would overdraw or exceed a limit is simply skipped).
    FOR i IN 1 .. COALESCE(array_length(t_debit, 1), 0) LOOP
        BEGIN
            IF t_state[i] = 'pending' THEN
                PERFORM request_transfer('seed-txn-' || i, aids[t_debit[i]], aids[t_credit[i]], t_amount[i], t_desc[i], 'transfer');
            ELSIF t_state[i] = 'canceled' THEN
                SELECT transfer_id INTO v_tid FROM request_transfer('seed-txn-' || i, aids[t_debit[i]], aids[t_credit[i]], t_amount[i], t_desc[i], 'transfer');
                PERFORM cancel_transfer(v_tid, 'seed: canceled');
            ELSIF t_state[i] = 'reversed' THEN
                SELECT transfer_id INTO v_tid FROM transfer('seed-txn-' || i, aids[t_debit[i]], aids[t_credit[i]], t_amount[i], t_desc[i], 'transfer');
                PERFORM reverse_transfer(v_tid, 'seed-rev-' || i, 'seed: reversed');
            ELSE
                PERFORM transfer('seed-txn-' || i, aids[t_debit[i]], aids[t_credit[i]], t_amount[i], t_desc[i], 'transfer');
            END IF;
        EXCEPTION WHEN OTHERS THEN
            NULL;
        END;
    END LOOP;

    RAISE NOTICE 'demo seed: %% customers, %% accounts, %% transactions attempted',
        array_length(usernames,1), array_length(a_iban,1), COALESCE(array_length(t_debit,1),0);
END $$;
`,
		users, accounts, realUsed, genUsed, len(ts), *nReasons,
		sqlText(usernames), sqlText(fullnames), sqlText(emails), sqlText(phones),
		sqlText(ibans), sqlInts(owner), sqlInt64s(deposit), sqlInt64s(limit),
		sqlInts(tDebit), sqlInts(tCredit), sqlInt64s(tAmount), sqlText(tDesc), sqlText(tState),
	)

	if err := os.WriteFile(*out, []byte(b.String()), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "write:", err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s (seed=%d): %d customers, %d accounts (%d real + %d generated), %d transactions\n",
		*out, seed, users, accounts, realUsed, genUsed, len(ts))
}

func loadValidIBANs(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil // no vendored pool -> all generated
	}
	seen := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		f, err := os.Open(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			s := iban.Normalize(sc.Text())
			if iban.IsValid(s) {
				seen[s] = true
			}
		}
		f.Close()
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func sqlText(ss []string) string {
	if len(ss) == 0 {
		return "ARRAY[]::TEXT[]"
	}
	parts := make([]string, len(ss))
	for i, s := range ss {
		parts[i] = "'" + strings.ReplaceAll(s, "'", "''") + "'"
	}
	return "ARRAY[" + wrap(parts) + "]"
}
func sqlInts(v []int) string {
	if len(v) == 0 {
		return "ARRAY[]::INT[]"
	}
	parts := make([]string, len(v))
	for i, n := range v {
		parts[i] = fmt.Sprintf("%d", n)
	}
	return "ARRAY[" + wrap(parts) + "]"
}
func sqlInt64s(v []int64) string {
	if len(v) == 0 {
		return "ARRAY[]::BIGINT[]"
	}
	parts := make([]string, len(v))
	for i, n := range v {
		parts[i] = fmt.Sprintf("%d", n)
	}
	return "ARRAY[" + wrap(parts) + "]"
}

func wrap(parts []string) string {
	var b strings.Builder
	for i, p := range parts {
		if i > 0 {
			b.WriteString(",")
			if i%10 == 0 {
				b.WriteString("\n        ")
			}
		}
		b.WriteString(p)
	}
	return b.String()
}
