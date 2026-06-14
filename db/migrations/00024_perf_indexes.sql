-- +goose Up

-- IDX-1 (the high-impact gap): ListMyTransfers — the client cross-account transaction
-- history — filters by the caller's accounts (debit OR credit) and orders by the
-- composite (requested_at, id) keyset cursor. The existing idx_transfers_debit/credit
-- (00004) key on created_at, so they don't back this ORDER BY; without these the query
-- join-and-sorts. These requested_at-keyed composites let each OR arm be an ordered
-- index range scan that matches the cursor directly.
CREATE INDEX idx_transfers_debit_req  ON transfers (debit_account_id, requested_at DESC, id DESC);
CREATE INDEX idx_transfers_credit_req ON transfers (credit_account_id, requested_at DESC, id DESC);

-- accounts.user_id: ListAccountsByUser (every customer's account list), the
-- /me account lookups, and set_default_account/create_account all filter accounts by
-- user_id with no supporting index (the one genuinely query-driven FK gap; the other
-- FKs the audit floated had no matching query at this scale).
CREATE INDEX idx_accounts_user ON accounts (user_id);

-- Drop three dead lower() btree indexes from 00004. They are superseded: full-text
-- search now uses the 00013 pg_trgm GIN indexes, and case-insensitive equality on
-- username/iban uses the citext columns' own (unique) indexes — nothing plans against
-- these lower(...) expressions anymore, so they are pure write/maintenance overhead.
DROP INDEX IF EXISTS idx_users_username_lower;
DROP INDEX IF EXISTS idx_users_fullname_lower;
DROP INDEX IF EXISTS idx_accounts_iban_lower;

-- +goose Down
CREATE INDEX idx_accounts_iban_lower   ON accounts (lower(iban));
CREATE INDEX idx_users_fullname_lower  ON users    (lower(full_name));
CREATE INDEX idx_users_username_lower  ON users    (lower(username));
DROP INDEX IF EXISTS idx_accounts_user;
DROP INDEX IF EXISTS idx_transfers_credit_req;
DROP INDEX IF EXISTS idx_transfers_debit_req;
