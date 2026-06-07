-- bank0 dev seed — idempotent (safe to re-run).
-- Uses the DB functions so all invariants/holds/idempotency apply.
-- Run:  psql "$DSN" -f db/seed.sql      (or via the compose `seed` service)
--
-- Shape: 5 customers, 2-3 accounts each (12 accounts total). Every account gets
-- 3 transactions: an opening deposit + one outgoing ring transfer + one incoming
-- ring transfer. Plus a few pending transfers for the operator queue.
--
-- Staff logins (dev passwords):
--   admin     / admin       (role admin, from migration 00011)
--   operator1 / operator    (role operator)
--   auditor1  / auditor      (role auditor)
-- Customers (alice/bob/carol/dave/erin) have no console access; password "password".

DO $$
DECLARE
    usernames  TEXT[] := ARRAY['alice', 'bob', 'carol', 'dave', 'erin'];
    fullnames  TEXT[] := ARRAY['Alice Andersson', 'Bob Bergström', 'Carol Carlsson', 'Dave Dahl', 'Erin Ek'];
    acctcounts INT[]  := ARRAY[3, 2, 3, 2, 2];   -- 12 accounts total
    aids       UUID[] := '{}';
    v_user     UUID;
    v_acct     UUID;
    v_iban     TEXT;
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
            v_iban := 'SE45' || lpad(seq::text, 20, '0');                 -- 24 chars, [A-Z0-9]
            SELECT id INTO v_acct FROM accounts WHERE iban = v_iban::varchar;
            IF v_acct IS NULL THEN
                v_acct := create_account(v_user, v_iban::varchar, lpad((1000 + seq)::text, 4, '0')::text, 50000::bigint);
            END IF;
            aids := array_append(aids, v_acct);

            -- transaction #1: opening deposit (€500)
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
END $$;

SELECT 'seed complete: ' ||
       (SELECT count(*) FROM users    WHERE role = 'customer') || ' customers, ' ||
       (SELECT count(*) FROM accounts WHERE kind = 'customer') || ' accounts, ' ||
       (SELECT count(*) FROM transfers) || ' transfers ('  ||
       (SELECT count(*) FROM transfers WHERE status = 'pending') || ' pending)' AS summary;
