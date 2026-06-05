-- +goose Up
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
    -- I1: balance cache matches the ledger
    SELECT 'balance_drift',
           format('account %s: cache=%s ledger=%s', a.id, a.balance_minor, l.s)
    FROM accounts a
    JOIN (SELECT account_id, COALESCE(SUM(signed_amount), 0) AS s
            FROM ledger_entries GROUP BY account_id) l ON l.account_id = a.id
    WHERE a.balance_minor <> l.s

    UNION ALL
    -- balance non-zero but no ledger rows at all (cache invented money)
    SELECT 'balance_without_ledger',
           format('account %s: cache=%s but no entries', a.id, a.balance_minor)
    FROM accounts a
    WHERE a.balance_minor <> 0
      AND NOT EXISTS (SELECT 1 FROM ledger_entries le WHERE le.account_id = a.id)

    UNION ALL
    -- I2: each transfer's legs net to zero
    SELECT 'transfer_unbalanced',
           format('transfer %s sums to %s', transfer_id, SUM(signed_amount))
    FROM ledger_entries GROUP BY transfer_id HAVING SUM(signed_amount) <> 0

    UNION ALL
    -- I3: global zero-sum (no money created or destroyed)
    SELECT 'global_nonzero',
           format('global ledger sums to %s', SUM(signed_amount))
    FROM ledger_entries HAVING SUM(signed_amount) <> 0;
$$ LANGUAGE sql STABLE;

-- +goose StatementEnd

-- +goose Down
DROP FUNCTION IF EXISTS reconcile();
DROP FUNCTION IF EXISTS cleanup_idempotency_keys();
DROP FUNCTION IF EXISTS expire_holds();
