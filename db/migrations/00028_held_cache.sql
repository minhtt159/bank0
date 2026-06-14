-- +goose Up
-- +goose StatementBegin

-- account_available was a per-row correlated subquery (balance - SUM(active holds))
-- in every account list. Make it O(1) the same way balance_minor is: a trigger-
-- maintained cache column. accounts.held_minor mirrors SUM(active holds); the holds
-- trigger keeps it in step, reconcile() proves it, and account_available becomes a
-- two-column read — so the four list/read queries get O(1) with zero query changes.
--
-- Scope note: this powers the READ/display path. The funds check in request_transfer
-- deliberately keeps computing a FRESH SUM — a money-authorizing decision should not
-- trust a cache, only display should.

ALTER TABLE accounts
    ADD COLUMN held_minor BIGINT NOT NULL DEFAULT 0 CHECK (held_minor >= 0);

-- Backfill from current active holds before the trigger takes over.
UPDATE accounts a
   SET held_minor = COALESCE(
       (SELECT SUM(h.amount_minor) FROM holds h
         WHERE h.account_id = a.id AND h.status = 'active'), 0);

-- holds_maintain_held: keep accounts.held_minor = SUM(active holds) per account.
-- A hold's amount_minor and account_id are immutable; only its status transitions
-- (active -> captured|released|expired), so we adjust on the active<->non-active edge.
-- AFTER trigger; it UPDATEs the (already-locked, in the money paths) account row.
CREATE OR REPLACE FUNCTION holds_maintain_held() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.status = 'active' THEN
            UPDATE accounts SET held_minor = held_minor + NEW.amount_minor WHERE id = NEW.account_id;
        END IF;
    ELSIF TG_OP = 'UPDATE' THEN
        IF OLD.status = 'active' AND NEW.status <> 'active' THEN
            UPDATE accounts SET held_minor = held_minor - OLD.amount_minor WHERE id = NEW.account_id;
        ELSIF OLD.status <> 'active' AND NEW.status = 'active' THEN
            UPDATE accounts SET held_minor = held_minor + NEW.amount_minor WHERE id = NEW.account_id;
        END IF;
    ELSIF TG_OP = 'DELETE' THEN
        IF OLD.status = 'active' THEN
            UPDATE accounts SET held_minor = held_minor - OLD.amount_minor WHERE id = OLD.account_id;
        END IF;
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_holds_maintain_held
    AFTER INSERT OR UPDATE OR DELETE ON holds
    FOR EACH ROW EXECUTE FUNCTION holds_maintain_held();

-- account_available is now an O(1) two-column read. Same signature, same result —
-- every caller (the four list/read queries) is unchanged and gets the speedup free.
CREATE OR REPLACE FUNCTION account_available(p_account_id UUID) RETURNS BIGINT AS $$
    SELECT balance_minor - held_minor FROM accounts WHERE id = p_account_id;
$$ LANGUAGE sql STABLE;

-- reconcile gains I4: the held cache must equal SUM(active holds), exactly as I1
-- guards the balance cache.
CREATE OR REPLACE FUNCTION reconcile() RETURNS TABLE (check_name TEXT, detail TEXT) AS $$
    SELECT 'balance_drift',
           format('account %s: cache=%s ledger=%s', a.id, a.balance_minor, l.s)
    FROM accounts a
    JOIN (SELECT account_id, COALESCE(SUM(signed_amount), 0) AS s
            FROM ledger_entries GROUP BY account_id) l ON l.account_id = a.id
    WHERE a.balance_minor <> l.s

    UNION ALL
    SELECT 'balance_without_ledger',
           format('account %s: cache=%s but no entries', a.id, a.balance_minor)
    FROM accounts a
    WHERE a.balance_minor <> 0
      AND NOT EXISTS (SELECT 1 FROM ledger_entries le WHERE le.account_id = a.id)

    UNION ALL
    SELECT 'transfer_unbalanced',
           format('transfer %s sums to %s', transfer_id, SUM(signed_amount))
    FROM ledger_entries GROUP BY transfer_id HAVING SUM(signed_amount) <> 0

    UNION ALL
    SELECT 'global_nonzero',
           format('global ledger sums to %s', SUM(signed_amount))
    FROM ledger_entries HAVING SUM(signed_amount) <> 0

    UNION ALL
    -- I4: held cache matches active holds
    SELECT 'held_drift',
           format('account %s: cache=%s holds=%s', a.id, a.held_minor, COALESCE(h.s, 0))
    FROM accounts a
    LEFT JOIN (SELECT account_id, SUM(amount_minor) AS s
                 FROM holds WHERE status = 'active' GROUP BY account_id) h ON h.account_id = a.id
    WHERE a.held_minor <> COALESCE(h.s, 0);
$$ LANGUAGE sql STABLE;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION reconcile() RETURNS TABLE (check_name TEXT, detail TEXT) AS $$
    SELECT 'balance_drift',
           format('account %s: cache=%s ledger=%s', a.id, a.balance_minor, l.s)
    FROM accounts a
    JOIN (SELECT account_id, COALESCE(SUM(signed_amount), 0) AS s
            FROM ledger_entries GROUP BY account_id) l ON l.account_id = a.id
    WHERE a.balance_minor <> l.s

    UNION ALL
    SELECT 'balance_without_ledger',
           format('account %s: cache=%s but no entries', a.id, a.balance_minor)
    FROM accounts a
    WHERE a.balance_minor <> 0
      AND NOT EXISTS (SELECT 1 FROM ledger_entries le WHERE le.account_id = a.id)

    UNION ALL
    SELECT 'transfer_unbalanced',
           format('transfer %s sums to %s', transfer_id, SUM(signed_amount))
    FROM ledger_entries GROUP BY transfer_id HAVING SUM(signed_amount) <> 0

    UNION ALL
    SELECT 'global_nonzero',
           format('global ledger sums to %s', SUM(signed_amount))
    FROM ledger_entries HAVING SUM(signed_amount) <> 0;
$$ LANGUAGE sql STABLE;

CREATE OR REPLACE FUNCTION account_available(p_account_id UUID) RETURNS BIGINT AS $$
    SELECT a.balance_minor
         - COALESCE((SELECT SUM(h.amount_minor) FROM holds h
                      WHERE h.account_id = a.id AND h.status = 'active'), 0)
    FROM accounts a
    WHERE a.id = p_account_id;
$$ LANGUAGE sql STABLE;
-- +goose StatementEnd

DROP TRIGGER IF EXISTS trg_holds_maintain_held ON holds;
DROP FUNCTION IF EXISTS holds_maintain_held();
ALTER TABLE accounts DROP COLUMN IF EXISTS held_minor;
