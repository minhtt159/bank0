-- +goose Up
-- Fuzzy search support: trigram matching (typo-tolerant) for users/accounts/
-- transfers. Substring ILIKE and word_similarity() both use these GIN indexes.
CREATE EXTENSION IF NOT EXISTS pg_trgm;

CREATE INDEX IF NOT EXISTS idx_users_username_trgm ON users    USING gin ((username::text) gin_trgm_ops);
CREATE INDEX IF NOT EXISTS idx_users_fullname_trgm ON users    USING gin (full_name        gin_trgm_ops);
CREATE INDEX IF NOT EXISTS idx_users_email_trgm    ON users    USING gin ((email::text)    gin_trgm_ops);
CREATE INDEX IF NOT EXISTS idx_accounts_iban_trgm  ON accounts USING gin ((iban::text)     gin_trgm_ops);
CREATE INDEX IF NOT EXISTS idx_transfers_desc_trgm ON transfers USING gin (description     gin_trgm_ops);

-- +goose Down
DROP INDEX IF EXISTS idx_transfers_desc_trgm;
DROP INDEX IF EXISTS idx_accounts_iban_trgm;
DROP INDEX IF EXISTS idx_users_email_trgm;
DROP INDEX IF EXISTS idx_users_fullname_trgm;
DROP INDEX IF EXISTS idx_users_username_trgm;
DROP EXTENSION IF EXISTS pg_trgm;
