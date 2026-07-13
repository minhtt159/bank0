-- bank0 default seed (1.0) — re-runnable on a POPULATED volume without error.
--
-- Re-run model: a SINGLE top-level guard. If the DB is already seeded (alice
-- exists) the main DO block emits a NOTICE and RETURNs — a clean no-op, never an
-- "idempotency key reused" error. On a FRESH DB the block runs exactly once and
-- seeds fully. Because the body runs at most once per DB, every money movement uses
-- a DYNAMIC idempotency key (gen_random_uuid()::text): there is no per-operation
-- NOT EXISTS guard and no coupling to the idempotency_keys table state. (The old
-- scheme pinned a fixed 'seed-…' key per movement so a re-run could top-up; that
-- bit a client dev re-running on a populated DB — keys reused with different
-- account uuids raise. The top-level guard makes the top-up machinery unnecessary.)
--
-- The staff users and the mule scenarios live in their OWN DO blocks, each still
-- individually NOT EXISTS / ON CONFLICT guarded — they're cheap config and worth
-- ensuring even on a partially-populated DB.
--
-- Run:  psql "$DSN" -f db/seed.sql      (or `task seed`)
--
-- Shape: ~89 customers (alice + 88), 2-3 accounts each (~200 accounts) — the
-- LIGHTWEIGHT default, a fraction of the heavy randomized demo seed (generated on
-- demand by `task seed:demo`); this just gives realistic volume so lists/statements
-- span pages. Usernames: alice + bob stay PINNED bare (docs + the e2e suite log in
-- as them); every other customer — the rest of the named personas, the generated
-- volume, and the mules — is <first>.<last> (matching seed_demo, with a numeric
-- suffix on the generated ones for uniqueness; all password "password"). Account
-- IBANs are structurally-realistic NL IBANs: a RANDOM 4-letter bank code
-- (ABNA/INGB/RABO/…) + a RANDOM 10-digit account number, iban_generate -> MOD-97
-- checksum (satisfies accounts_iban_checksum). Every account gets an
-- opening deposit + a ring transfer (out and in). Plus a handful of pending
-- transfers for the operator queue and one canceled + one reversed transfer so the
-- lifecycle states are all represented, and a long bulk run on alice's first
-- account so its statement pages.
--
-- Staff logins (dev passwords):
--   admin     / admin       (role admin, from migration 00009_system_seed)
--   operator1 / operator    (role operator)
--   auditor1  / auditor      (role auditor)
-- The customers have no console access; password "password".

--- staff (own block, independently guarded — cheap config) ----------------
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM users WHERE username = 'operator1'::citext) THEN
        PERFORM create_user('operator1', 'operator', 'Olivia Operator', 'operator1@bank0.test', '+46700000010', 'operator');
    END IF;
    IF NOT EXISTS (SELECT 1 FROM users WHERE username = 'auditor1'::citext) THEN
        PERFORM create_user('auditor1', 'auditor', 'Aaron Auditor', 'auditor1@bank0.test', '+46700000011', 'auditor');
    END IF;
END $$;

-- Session-local helper: derive a <first>.<last> login from a full name, e.g.
-- 'Carol Carlsson' -> 'carol.carlsson', 'Ines de Boer' -> 'ines.deboer'. Used for
-- every customer except the pinned alice/bob. Auto-dropped at session end.
CREATE OR REPLACE FUNCTION pg_temp.username_of(full_name text) RETURNS text
    LANGUAGE sql IMMUTABLE AS $fn$
    SELECT lower(split_part(full_name, ' ', 1)) || '.' ||
           lower(replace(substring(full_name FROM position(' ' IN full_name) + 1), ' ', ''));
$fn$;

--- customers + accounts + transfers (run-once) ----------------------------
-- Top-level guard: if alice exists the DB is already seeded — emit a NOTICE and
-- RETURN. Everything below therefore runs at most ONCE per DB, so the money
-- movements can use dynamic gen_random_uuid() idempotency keys with no per-op
-- guard: a re-run never reaches them.
DO $$
DECLARE
    -- The 30 NAMED demo personas (alice/bob/carol… — the e2e suite + guided demo
    -- pin these specific usernames). Do not reorder or rename these.
    named_users TEXT[] := ARRAY[
        'alice', 'bob', 'carol', 'dave', 'erin', 'frank', 'grace', 'henrik', 'ines', 'jonas',
        'klara', 'lars', 'maja', 'niklas', 'olga', 'pavel', 'quinn', 'rosa', 'sven', 'tara',
        'ulrik', 'vera', 'wouter', 'xenia', 'yusuf', 'zara', 'anton', 'bea', 'cleo', 'dario'];
    named_names TEXT[] := ARRAY[
        'Alice Andersson', 'Bob Bergström', 'Carol Carlsson', 'Dave Dahl', 'Erin Ek',
        'Frank Fischer', 'Grace Visser', 'Henrik Jansen', 'Ines de Boer', 'Jonas Bakker',
        'Klara Mulder', 'Lars de Vries', 'Maja Smit', 'Niklas Meijer', 'Olga Bos',
        'Pavel Novak', 'Quinn de Jong', 'Rosa Vermeulen', 'Sven Hendriks', 'Tara van Dijk',
        'Ulrik Dekker', 'Vera van den Berg', 'Wouter Peters', 'Xenia Kuipers', 'Yusuf Demir',
        'Zara van Leeuwen', 'Anton Schouten', 'Bea Willems', 'Cleo Maas', 'Dario Romano'];
    -- Name parts for the extra generated customers (cust31…). Combined
    -- deterministically so each username maps to a stable full name.
    gen_first TEXT[] := ARRAY[
        'Emma', 'Lucas', 'Sara', 'Daan', 'Julia', 'Sem', 'Tess', 'Finn', 'Noor', 'Liam',
        'Eva', 'Bram', 'Lotte', 'Thijs', 'Sophie', 'Ruben', 'Anna', 'Jesse', 'Fleur', 'Tim',
        'Lena', 'Joost', 'Mila', 'Stijn', 'Iris', 'Gijs', 'Sanne', 'Teun', 'Roos', 'Cas'];
    gen_last  TEXT[] := ARRAY[
        'de Wit', 'van der Berg', 'Janssen', 'Visser', 'Kok', 'Bakker', 'de Groot', 'Vos',
        'Mulder', 'Bos', 'Peters', 'Hendriks', 'van Dijk', 'de Jong', 'Smit', 'Brouwer',
        'van Leeuwen', 'Dijkstra', 'Postma', 'Kuiper', 'Veenstra', 'Prins', 'Huisman',
        'van der Heijden', 'Schipper', 'Maas', 'Verhoeven', 'Koster', 'Willems', 'Timmermans'];
    -- Total customers to provision (30 named + the rest generated). Bumping n_total
    -- and n_bulk below is the one knob to grow the default volume.
    n_named    INT := 30;
    n_total    INT := 88;            -- 30 named + 58 generated -> ~200 accounts
    -- Real NL bank codes, cycled, so the BBAN is bank-code + 10-digit account number
    -- (a realistic NL IBAN) rather than 14 raw digits.
    bankcodes  TEXT[] := ARRAY['ABNA', 'INGB', 'RABO', 'TRIO', 'SNSB', 'KNAB'];
    aids       UUID[] := '{}';
    v_username TEXT;
    v_fullname TEXT;
    v_naccts   INT;
    v_user     UUID;
    v_acct     UUID;
    v_iban     TEXT;
    v_tid      UUID;
    n_bulk     INT := 280;           -- bulk transfers on alice's first account
    seq        INT := 1;
    i          INT;
    j          INT;
    n          INT;
BEGIN
    -- ── top-level run-once guard ────────────────────────────────────────────
    -- A populated DB is a clean no-op (never an idempotency error). Only a fresh
    -- DB (no alice) runs the body — exactly once — so dynamic keys are safe below.
    IF EXISTS (SELECT 1 FROM users WHERE username = 'alice'::citext) THEN
        RAISE NOTICE 'bank0 already seeded — skipping';
        RETURN;
    END IF;

    --- customers + accounts + opening deposits -----------------------------
    -- The first n_named use the pinned named arrays; the rest get a deterministic
    -- username (custNN) + a synthesized full name. Account count cycles 2-3 per
    -- customer so the totals stay coherent (~2.2 accounts/customer).
    FOR i IN 1 .. n_total LOOP
        IF i <= n_named THEN
            v_fullname := named_names[i];
            IF i <= 2 THEN
                v_username := named_users[i];                   -- alice, bob: pinned bare (docs + e2e)
            ELSE
                v_username := pg_temp.username_of(v_fullname);  -- carol… -> first.last
            END IF;
        ELSE
            -- combine a first + last part by independent cycles for variety
            v_fullname := gen_first[1 + ((i - 1) % array_length(gen_first, 1))] || ' ' ||
                          gen_last[1 + ((i * 7) % array_length(gen_last, 1))];
            v_username := pg_temp.username_of(v_fullname) || i; -- first.last<i> (unique, seed_demo style)
        END IF;

        -- 2-3 accounts: customers at positions ≡1 or ≡3 (mod 5) get 3, the rest 2.
        v_naccts := CASE WHEN i % 5 IN (1, 3) THEN 3 ELSE 2 END;

        v_user := create_user(v_username::citext, 'password'::text, v_fullname::text,
                              (v_username || '@example.com')::citext, NULL::varchar, 'customer'::user_role);

        FOR j IN 1 .. v_naccts LOOP
            -- realistic NL BBAN: RANDOM 4-letter bank code + RANDOM 10-digit account
            -- number (iban_generate fills the MOD-97 checksum). Retry guards the rare
            -- collision on the UNIQUE accounts.iban.
            LOOP
                v_iban := iban_generate('NL', bankcodes[1 + floor(random() * array_length(bankcodes, 1))::int]
                                              || lpad((floor(random() * 1e10))::bigint::text, 10, '0'));
                EXIT WHEN NOT EXISTS (SELECT 1 FROM accounts WHERE iban = v_iban::varchar);
            END LOOP;
            v_acct := create_account(v_user, v_iban::varchar, lpad((1000 + seq)::text, 4, '0')::text, 50000::bigint);
            -- opening deposit (€500). Dynamic key — this section runs at most once.
            PERFORM deposit(gen_random_uuid()::text, v_acct, 50000, 'Opening deposit');
            aids := array_append(aids, v_acct);
            seq := seq + 1;
        END LOOP;
    END LOOP;

    --- ring transfers: each account sends to the next (so it also receives
    --- from the previous). With the deposit, that's 3 transactions per account.
    n := array_length(aids, 1);
    FOR i IN 1 .. n LOOP
        PERFORM transfer(gen_random_uuid()::text, aids[i], aids[(i % n) + 1], 1000,
                         'Seed transfer #' || i, 'transfer');
    END LOOP;

    --- a few pending transfers for the operator queue ----------------------
    PERFORM request_transfer(gen_random_uuid()::text, aids[1],  aids[4],  2500, 'Deferred: pending demo 1', 'transfer');
    PERFORM request_transfer(gen_random_uuid()::text, aids[5],  aids[2],  1500, 'Deferred: pending demo 2', 'transfer');
    PERFORM request_transfer(gen_random_uuid()::text, aids[7],  aids[10], 3000, 'Deferred: pending demo 3', 'transfer');
    PERFORM request_transfer(gen_random_uuid()::text, aids[20], aids[33], 1800, 'Deferred: pending demo 4', 'transfer');

    --- one canceled + one reversed, so every lifecycle state is present ----
    SELECT transfer_id INTO v_tid
      FROM request_transfer(gen_random_uuid()::text, aids[3], aids[15], 2000, 'Canceled demo', 'transfer');
    PERFORM cancel_transfer(v_tid, 'seed: canceled demo');

    SELECT transfer_id INTO v_tid
      FROM transfer(gen_random_uuid()::text, aids[8], aids[22], 1200, 'Reversed demo', 'transfer');
    PERFORM reverse_transfer(v_tid, gen_random_uuid()::text, 'seed: reversed demo');

    --- bulk volume so lists + the first account's statement span pages.
    --- aids[1] is in every bulk transfer (alternating side) -> a long statement.
    --- Amounts stay tiny (€1-3) so alice's first account never runs low even across
    --- hundreds of outgoing legs (opening €500 + the ring/bulk inflows cover it).
    FOR i IN 1 .. n_bulk LOOP
        IF i % 2 = 0 THEN
            PERFORM transfer(gen_random_uuid()::text, aids[1], aids[(i % (n - 1)) + 2],
                             100 * (1 + i % 3), 'Bulk #' || i || ' out', 'transfer');
        ELSE
            PERFORM transfer(gen_random_uuid()::text, aids[(i % (n - 1)) + 2], aids[1],
                             100 * (1 + i % 3), 'Bulk #' || i || ' in', 'transfer');
        END IF;
    END LOOP;
END $$;

--- guided-transfer demo: a POOL of mule accounts for the APP-scam "mule menu" ------
-- GET /transfers/suggestion (resolver in 00008_features) draws up to 3 RANDOM
-- third-party targets from the ACTIVE guided_scenarios short-list; without one it
-- falls back to the caller's OWN account (a safe self-transfer, not a scam). To make
-- the draw varied, seed a POOL: 10 dedicated mule customers holding 30 accounts
-- between them (each user >= 1), every account fronted by one active GLOBAL scenario
-- (target_user_id NULL = any caller) so the menu can surface any of the 30. Maximally
-- randomized — names, usernames (first.last<k>, like seed_demo), BBANs, owner
-- distribution, opening deposits, PINs, limits, and the payee "reason". bob/carol are
-- ordinary customers (not decoys). Own block, idempotent on re-run keyed by the stable
-- scenario name app-scam-mule-1 (random IBANs can't be re-derived to dedupe by).
DO $$
DECLARE
    n_users   INT := 10;     -- dedicated mule customers
    n_accts   INT := 30;     -- mule accounts spread across them (each user >= 1)
    fnames    TEXT[] := ARRAY['Markus','Saga','Joran','Elin','Tobias','Freya','Anders','Nadia','Viktor','Lina',
                              'Sven','Maja','Kasper','Ingrid','Lars','Sofia','Mikkel','Astrid','Rune','Helena'];
    lnames    TEXT[] := ARRAY['Eklund','Lindqvist','Visser','Berg','Holm','Dahl','Nyman','Falk','Sandberg','Ek',
                              'Lund','Strom','Aaltonen','Bakker','Vos','Jansen','Larsson','Moller','Haugen','Voss'];
    reasons   TEXT[] := ARRAY['Recommended payee','Trusted payee','Verified payee','Saved payee','Frequent payee','Known recipient','Your contact'];
    bankcodes TEXT[] := ARRAY['ABNA','INGB','RABO','TRIO','SNSB','KNAB'];
    mules     UUID[] := '{}';
    v_owner   UUID;
    v_acct    UUID;
    v_iban    TEXT;
    v_uname   TEXT;
    v_full    TEXT;
    k         INT;
    a         INT;
BEGIN
    -- Idempotent: the scenario names app-scam-mule-1..N are the stable key. If the
    -- first exists, the pool was already seeded on a prior run, so skip it all.
    IF EXISTS (SELECT 1 FROM guided_scenarios WHERE name = 'app-scam-mule-1') THEN
        RETURN;
    END IF;

    -- 10 mule customers — random first.last<k> names (k keeps the username unique).
    FOR k IN 1 .. n_users LOOP
        v_full  := fnames[1 + floor(random() * array_length(fnames, 1))::int] || ' ' ||
                   lnames[1 + floor(random() * array_length(lnames, 1))::int];
        v_uname := pg_temp.username_of(v_full) || k;
        mules := array_append(mules,
                 create_user(v_uname::citext, 'password'::text, v_full::text,
                             (v_uname || '@example.com')::citext, NULL::varchar, 'customer'::user_role));
    END LOOP;

    -- 30 mule accounts: each user gets >= 1 (accounts 1..10), the rest go to a RANDOM
    -- mule. Random BBAN, PIN, transfer limit, and opening deposit; one active global
    -- scenario per account so the menu draws from all 30.
    FOR a IN 1 .. n_accts LOOP
        IF a <= n_users THEN
            v_owner := mules[a];                                                   -- guarantee each user funded
        ELSE
            v_owner := mules[1 + floor(random() * array_length(mules, 1))::int];   -- random owner
        END IF;

        LOOP
            v_iban := iban_generate('NL', bankcodes[1 + floor(random() * array_length(bankcodes, 1))::int]
                                          || lpad((floor(random() * 1e10))::bigint::text, 10, '0'));
            EXIT WHEN NOT EXISTS (SELECT 1 FROM accounts WHERE iban = v_iban::varchar);
        END LOOP;

        v_acct := create_account(v_owner, v_iban::varchar,
                                 lpad((1000 + floor(random() * 9000))::int::text, 4, '0')::text,  -- random PIN
                                 ((1 + floor(random() * 10)) * 50000)::bigint);                    -- random limit €500..€5000
        PERFORM deposit(gen_random_uuid()::text, v_acct,
                        ((10 + floor(random() * 990)) * 100)::bigint, 'Opening deposit');           -- random €10..€999

        INSERT INTO guided_scenarios (name, target_account_id, reason, target_user_id, min_amount_minor, priority, active)
        VALUES ('app-scam-mule-' || a, v_acct,
                reasons[1 + floor(random() * array_length(reasons, 1))::int], NULL, 0, 100, TRUE);
    END LOOP;
END $$;

--- fraud/AML demo policy: warning_rules + watchlist (Recs 22/25) ------------------
-- The fraud gate in transfer() is inert until rules/watchlist exist (both tables
-- ship EMPTY). Seed a small demo set so the client-path gate is demonstrable:
--   * destination_flagged -> review (parks the payment 'held' for confirmation),
--     with a cooling-off + required acknowledgement (the VOP liability pivot);
--   * a HIGH assessed band -> warn + required ack;
--   * a first payment to a new payee -> a soft warn (no ack).
-- Watchlist entries name deterministic seeded personas (Grace Visser, Pavel Novak)
-- so a payment to/from them parks 'under_review' for operator screening. Own block,
-- idempotent: keyed on the stable rule headline / watchlist reason. Sentinel-caller
-- seed transfers above are NOT gated (only real client-path payments are), so this
-- never perturbs the seeded ledger.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM warning_rules WHERE headline = 'Possible scam payment') THEN
        INSERT INTO warning_rules
            (match_reason_code, match_min_band, category, headline, body, severity,
             decision, required_ack, cooling_off_seconds, priority, active)
        VALUES
            ('destination_flagged', NULL, 'risk_warning', 'Possible scam payment',
             'This account has been reported or flagged. Scammers pressure you to ignore warnings. Please pause and check.',
             'critical', 'review', TRUE, 30, 100, TRUE),
            (NULL, 'high', 'risk_warning', 'This payment looks risky',
             'Several signals suggest this payment is unusual for you. Confirm you know and trust the recipient.',
             'warning', 'warn', TRUE, 0, 50, TRUE),
            ('first_payment_to_payee', NULL, 'risk_warning', 'First payment to this payee',
             'This is your first payment to this account. Double-check the details before you send.',
             'info', 'warn', FALSE, 0, 10, TRUE);
    END IF;

    IF NOT EXISTS (SELECT 1 FROM watchlist_entries WHERE reason = 'demo: sanctions screening') THEN
        INSERT INTO watchlist_entries (pattern, reason, active) VALUES
            ('%Visser%', 'demo: sanctions screening', TRUE),
            ('%Novak%',  'demo: sanctions screening', TRUE);
    END IF;
END $$;

SELECT 'seed complete: ' ||
       (SELECT count(*) FROM users    WHERE role = 'customer') || ' customers, ' ||
       (SELECT count(*) FROM accounts WHERE kind = 'customer') || ' accounts, ' ||
       (SELECT count(*) FROM transfers) || ' transfers ('  ||
       (SELECT count(*) FROM transfers WHERE status = 'pending') || ' pending), ' ||
       (SELECT count(*) FROM guided_scenarios WHERE active) || ' active mule scenarios' AS summary;
