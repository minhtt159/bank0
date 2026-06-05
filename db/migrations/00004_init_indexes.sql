-- +goose Up

-- Account statement pagination (cursor on posted_at, id). id is UUIDv7 -> time-ordered tiebreak.
CREATE INDEX idx_ledger_account_posted ON ledger_entries (account_id, posted_at DESC, id DESC);
-- Fetch both legs of a transfer.
CREATE INDEX idx_ledger_transfer       ON ledger_entries (transfer_id);

-- available-balance computation and expiry sweep (partial: active holds only).
CREATE INDEX idx_holds_active_account  ON holds (account_id) WHERE status = 'active';
CREATE INDEX idx_holds_expiry          ON holds (expires_at) WHERE status = 'active';

-- operator "pending queue".
CREATE INDEX idx_transfers_pending     ON transfers (requested_at) WHERE status = 'pending';
-- per-account transfer history (the tf-backend UNION ALL pattern hits these independently).
CREATE INDEX idx_transfers_debit       ON transfers (debit_account_id, created_at DESC);
CREATE INDEX idx_transfers_credit      ON transfers (credit_account_id, created_at DESC);

-- idempotency key TTL cleanup.
CREATE INDEX idx_idempotency_expiry    ON idempotency_keys (expires_at);

-- search (ILIKE) — swap for pg_trgm GIN later if needed.
CREATE INDEX idx_users_username_lower  ON users    (lower(username));
CREATE INDEX idx_users_fullname_lower  ON users    (lower(full_name));
CREATE INDEX idx_accounts_iban_lower   ON accounts (lower(iban));

-- audit log browsing.
CREATE INDEX idx_admin_actions_created ON admin_actions (created_at DESC);

-- +goose Down
DROP INDEX IF EXISTS idx_admin_actions_created;
DROP INDEX IF EXISTS idx_accounts_iban_lower;
DROP INDEX IF EXISTS idx_users_fullname_lower;
DROP INDEX IF EXISTS idx_users_username_lower;
DROP INDEX IF EXISTS idx_idempotency_expiry;
DROP INDEX IF EXISTS idx_transfers_credit;
DROP INDEX IF EXISTS idx_transfers_debit;
DROP INDEX IF EXISTS idx_transfers_pending;
DROP INDEX IF EXISTS idx_holds_expiry;
DROP INDEX IF EXISTS idx_holds_active_account;
DROP INDEX IF EXISTS idx_ledger_transfer;
DROP INDEX IF EXISTS idx_ledger_account_posted;
