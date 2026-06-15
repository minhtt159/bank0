-- +goose Up
-- ─────────────────────────────────────────────────────────────────────────────
-- MAINTENANCE & RECONCILIATION — sweeps, invariant checks, and the read model
-- The last slice of the former core_banking monolith. Holds the periodic sweeps
-- (expire_holds, cleanup_idempotency_keys), reconcile() (asserts the ledger
-- correctness invariants — empty result = books are correct), and enriched_ledger
-- (the human-readable statement/console read model).
--
-- Depends on the accounts/holds (00004) and transfers/ledger (00005) tables; the
-- view also joins users (00003).
-- ─────────────────────────────────────────────────────────────────────────────

-- +goose StatementBegin

-- expire_holds: batch sweep. Active holds past expiry -> expired; their pending
-- transfers -> failed. Run by the app's ticker (or pg_cron in production).
CREATE OR REPLACE FUNCTION expire_holds() RETURNS INTEGER AS $$
DECLARE v_n INTEGER;
BEGIN
    WITH expired AS (
        UPDATE holds SET status = 'expired', released_at = now()
        WHERE status = 'active' AND expires_at < now()
        RETURNING transfer_id
    )
    UPDATE transfers SET status = 'failed', failure_reason = 'hold expired'
    WHERE id IN (SELECT transfer_id FROM expired) AND status = 'pending';
    GET DIAGNOSTICS v_n = ROW_COUNT;
    RETURN v_n;
END;
$$ LANGUAGE plpgsql;

-- cleanup_idempotency_keys: drop completed keys past their TTL.
CREATE OR REPLACE FUNCTION cleanup_idempotency_keys() RETURNS INTEGER AS $$
DECLARE v_n INTEGER;
BEGIN
    DELETE FROM idempotency_keys WHERE status = 'completed' AND expires_at < now();
    GET DIAGNOSTICS v_n = ROW_COUNT;
    RETURN v_n;
END;
$$ LANGUAGE plpgsql;

-- reconcile: assert the correctness invariants. Empty result = books are correct.
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

-- ─────────────────────────────────────────────────────────────────────────────
-- enriched_ledger: the human-readable read model for statements and the operator
-- console. One row per posting, with the account, the transfer, and the
-- counterparty resolved. Reads from the single source of truth (the ledger).
-- ─────────────────────────────────────────────────────────────────────────────
CREATE VIEW enriched_ledger AS
SELECT
    le.id,
    le.transfer_id,
    le.account_id,
    a.iban                AS account_iban,
    a.system_code        AS account_system_code,
    au.full_name         AS account_owner,
    le.direction,
    le.amount_minor,
    le.signed_amount,
    le.balance_after,
    le.currency,
    le.posted_at,
    t.kind               AS transfer_kind,
    t.status             AS transfer_status,
    t.description,
    t.reverses_id,
    cp.id                AS counterparty_account_id,
    cp.iban              AS counterparty_iban,
    cp.system_code       AS counterparty_system_code,
    cu.full_name         AS counterparty_owner
FROM ledger_entries le
JOIN transfers t ON t.id = le.transfer_id
JOIN accounts  a ON a.id = le.account_id
LEFT JOIN users au ON au.id = a.user_id
JOIN accounts cp ON cp.id = CASE WHEN le.account_id = t.debit_account_id
                                 THEN t.credit_account_id ELSE t.debit_account_id END
LEFT JOIN users cu ON cu.id = cp.user_id;

-- +goose Down
DROP VIEW IF EXISTS enriched_ledger;
-- +goose StatementBegin
DROP FUNCTION IF EXISTS reconcile();
DROP FUNCTION IF EXISTS cleanup_idempotency_keys();
DROP FUNCTION IF EXISTS expire_holds();
-- +goose StatementEnd
