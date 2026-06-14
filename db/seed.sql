-- bank0 dev seed — idempotent (safe to re-run).
-- Uses the DB functions so all invariants/holds/idempotency apply.
-- Run:  psql "$DSN" -f db/seed.sql      (or via the compose `seed` service)
--
-- Shape: 30 customers, 1-3 accounts each (72 accounts total) — ~5-10% of the
-- generated demo seed (db/seed_demo.sql). Account IBANs are structurally-realistic
-- NL IBANs: a real 4-letter bank code (ABNA/INGB/RABO/…) + a 10-digit account number,
-- iban_generate -> MOD-97 checksum (satisfies the accounts_iban_checksum CHECK).
-- Every account gets an opening deposit + a ring transfer (out and in). Plus a
-- handful of pending transfers for the operator queue and one canceled + one
-- reversed transfer so the lifecycle states are all represented. The richer,
-- randomized data set lives in db/seed_demo.sql (`task seed:demo`).
--
-- Staff logins (dev passwords):
--   admin     / admin       (role admin, from migration 00011)
--   operator1 / operator    (role operator)
--   auditor1  / auditor      (role auditor)
-- The 30 customers (alice/bob/carol/dave/erin/frank + 24 more) have no console
-- access; password "password".

DO $$
DECLARE
    usernames  TEXT[] := ARRAY[
        'alice', 'bob', 'carol', 'dave', 'erin', 'frank', 'grace', 'henrik', 'ines', 'jonas',
        'klara', 'lars', 'maja', 'niklas', 'olga', 'pavel', 'quinn', 'rosa', 'sven', 'tara',
        'ulrik', 'vera', 'wouter', 'xenia', 'yusuf', 'zara', 'anton', 'bea', 'cleo', 'dario'];
    fullnames  TEXT[] := ARRAY[
        'Alice Andersson', 'Bob Bergström', 'Carol Carlsson', 'Dave Dahl', 'Erin Ek',
        'Frank Fischer', 'Grace Visser', 'Henrik Jansen', 'Ines de Boer', 'Jonas Bakker',
        'Klara Mulder', 'Lars de Vries', 'Maja Smit', 'Niklas Meijer', 'Olga Bos',
        'Pavel Novak', 'Quinn de Jong', 'Rosa Vermeulen', 'Sven Hendriks', 'Tara van Dijk',
        'Ulrik Dekker', 'Vera van den Berg', 'Wouter Peters', 'Xenia Kuipers', 'Yusuf Demir',
        'Zara van Leeuwen', 'Anton Schouten', 'Bea Willems', 'Cleo Maas', 'Dario Romano'];
    -- 1-3 accounts per customer; sums to 72.
    acctcounts INT[]  := ARRAY[
        3, 2, 3, 2, 2, 3, 2, 3, 2, 2,
        3, 2, 3, 2, 2, 3, 2, 3, 2, 2,
        3, 2, 3, 2, 2, 3, 2, 3, 2, 2];
    -- Real NL bank codes, cycled, so the BBAN is bank-code + 10-digit account number
    -- (a realistic NL IBAN) rather than 14 raw digits.
    bankcodes  TEXT[] := ARRAY['ABNA', 'INGB', 'RABO', 'TRIO', 'SNSB', 'KNAB'];
    aids       UUID[] := '{}';
    v_user     UUID;
    v_acct     UUID;
    v_iban     TEXT;
    v_tid      UUID;
    seq        INT := 1;
    i          INT;
    j          INT;
    n          INT;
BEGIN
    --- staff ---------------------------------------------------------------
    IF NOT EXISTS (SELECT 1 FROM users WHERE username = 'operator1') THEN
        PERFORM create_user('operator1', 'operator', 'Olivia Operator', 'operator1@bank0.test', '+46700000010', 'operator');
    END IF;
    IF NOT EXISTS (SELECT 1 FROM users WHERE username = 'auditor1') THEN
        PERFORM create_user('auditor1', 'auditor', 'Aaron Auditor', 'auditor1@bank0.test', '+46700000011', 'auditor');
    END IF;

    --- customers + accounts + opening deposits -----------------------------
    FOR i IN 1 .. array_length(usernames, 1) LOOP
        SELECT id INTO v_user FROM users WHERE username = usernames[i]::citext;
        IF v_user IS NULL THEN
            v_user := create_user(usernames[i]::citext, 'password'::text, fullnames[i]::text,
                                  (usernames[i] || '@example.com')::citext, NULL::varchar, 'customer'::user_role);
        END IF;

        FOR j IN 1 .. acctcounts[i] LOOP
            -- realistic NL BBAN: 4-letter bank code + 10-digit account number.
            v_iban := iban_generate('NL', bankcodes[1 + (seq % array_length(bankcodes, 1))] || lpad(seq::text, 10, '0'));
            SELECT id INTO v_acct FROM accounts WHERE iban = v_iban::varchar;
            IF v_acct IS NULL THEN
                v_acct := create_account(v_user, v_iban::varchar, lpad((1000 + seq)::text, 4, '0')::text, 50000::bigint);
            END IF;
            aids := array_append(aids, v_acct);

            -- opening deposit (€500)
            PERFORM deposit('seed-dep-' || v_iban, v_acct, 50000, 'Opening deposit');
            seq := seq + 1;
        END LOOP;
    END LOOP;

    --- ring transfers: each account sends to the next (so it also receives
    --- from the previous). With the deposit, that's 3 transactions per account.
    n := array_length(aids, 1);
    FOR i IN 1 .. n LOOP
        PERFORM transfer('seed-ring-' || i, aids[i], aids[(i % n) + 1], 1000,
                         'Seed transfer #' || i, 'transfer');
    END LOOP;

    --- a few pending transfers for the operator queue ----------------------
    PERFORM request_transfer('seed-pend-1', aids[1],  aids[4],  2500, 'Deferred: pending demo 1', 'transfer');
    PERFORM request_transfer('seed-pend-2', aids[5],  aids[2],  1500, 'Deferred: pending demo 2', 'transfer');
    PERFORM request_transfer('seed-pend-3', aids[7],  aids[10], 3000, 'Deferred: pending demo 3', 'transfer');
    PERFORM request_transfer('seed-pend-4', aids[20], aids[33], 1800, 'Deferred: pending demo 4', 'transfer');

    --- one canceled + one reversed, so every lifecycle state is present ----
    SELECT transfer_id INTO v_tid
      FROM request_transfer('seed-cancel-1', aids[3], aids[15], 2000, 'Canceled demo', 'transfer');
    PERFORM cancel_transfer(v_tid, 'seed: canceled demo');

    SELECT transfer_id INTO v_tid
      FROM transfer('seed-rev-src', aids[8], aids[22], 1200, 'Reversed demo', 'transfer');
    PERFORM reverse_transfer(v_tid, 'seed-rev-1', 'seed: reversed demo');

    --- bulk volume so lists + the first account's statement span pages.
    --- aids[1] is in every bulk transfer (alternating side) -> a long statement.
    FOR i IN 1 .. 90 LOOP
        IF i % 2 = 0 THEN
            PERFORM transfer('seed-bulk-' || i, aids[1], aids[(i % (n - 1)) + 2],
                             100 * (1 + i % 3), 'Bulk #' || i || ' out', 'transfer');
        ELSE
            PERFORM transfer('seed-bulk-' || i, aids[(i % (n - 1)) + 2], aids[1],
                             100 * (1 + i % 3), 'Bulk #' || i || ' in', 'transfer');
        END IF;
    END LOOP;
END $$;

--- guided-transfer demo: an APP-scam "mule" steer --------------------------
-- Without an active guided_scenario (00019), GET /transfers/suggestion falls
-- back to the caller's OWN other account — a safe self-transfer, not a realistic
-- scam. Seed a dedicated "mule" customer + account and a GLOBAL scenario so
-- guided mode dictates a THIRD-PARTY recipient for every caller: the authorised-
-- push-payment steer fraudbank's guided mode demonstrates. Idempotent.
DO $$
DECLARE
    v_mule_user UUID;
    v_mule_acct UUID;
    v_iban      TEXT := iban_generate('NL', 'KNAB' || lpad('9000000099', 10, '0'));
BEGIN
    SELECT id INTO v_mule_user FROM users WHERE username = 'mule'::citext;
    IF v_mule_user IS NULL THEN
        v_mule_user := create_user('mule'::citext, 'password'::text, 'Markus Eklund'::text,
                                   'mule@example.com'::citext, NULL::varchar, 'customer'::user_role);
    END IF;

    SELECT id INTO v_mule_acct FROM accounts WHERE iban = v_iban::varchar;
    IF v_mule_acct IS NULL THEN
        v_mule_acct := create_account(v_mule_user, v_iban::varchar, '9099'::text, 50000::bigint);
        PERFORM deposit('seed-dep-' || v_iban, v_mule_acct, 50000, 'Opening deposit');
    END IF;

    -- Global scenario (target_user_id NULL => any caller). The own-account fallback
    -- has no scenario row, so this always wins. Never returned to the mule's own
    -- logins (the resolver excludes the debit account; fraudbank also guards
    -- against any own-account suggestion).
    INSERT INTO guided_scenarios (name, target_account_id, reason, target_user_id, min_amount_minor, priority, active)
    SELECT 'app-scam-demo', v_mule_acct, 'Recommended payee', NULL, 0, 100, TRUE
     WHERE NOT EXISTS (SELECT 1 FROM guided_scenarios WHERE name = 'app-scam-demo');
END $$;

SELECT 'seed complete: ' ||
       (SELECT count(*) FROM users    WHERE role = 'customer') || ' customers, ' ||
       (SELECT count(*) FROM accounts WHERE kind = 'customer') || ' accounts, ' ||
       (SELECT count(*) FROM transfers) || ' transfers ('  ||
       (SELECT count(*) FROM transfers WHERE status = 'pending') || ' pending)' AS summary;
