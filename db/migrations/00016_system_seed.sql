-- +goose Up
-- ─────────────────────────────────────────────────────────────────────────────
-- SYSTEM SEED — structural rows the schema cannot run without
-- The general-ledger (system) accounts are STRUCTURAL: money can only enter/leave
-- the bank through them, so they are seeded as part of the schema, not as demo data.
-- Plus a bootstrap admin operator so a fresh deployment has someone who can log in.
-- (Demo/sample customers and transactions live in db/seed.sql, not here.)
-- ─────────────────────────────────────────────────────────────────────────────

INSERT INTO accounts (kind, system_code, currency, status) VALUES
    ('system', 'EXTERNAL_CLEARING', 'EUR', 'active'),  -- boundary: deposits/withdrawals
    ('system', 'CASH',              'EUR', 'active'),   -- physical cash drawer
    ('system', 'FEES',             'EUR', 'active')     -- fee income
ON CONFLICT (system_code) DO NOTHING;

-- Bootstrap admin operator (PoC convenience — change the password immediately).
INSERT INTO users (username, password_hash, full_name, role)
VALUES ('admin', crypt('admin', gen_salt('bf', 10)), 'Administrator', 'admin')
ON CONFLICT (username) DO NOTHING;

-- +goose Down
-- Best-effort, and only clean on a DB that never traded: once the admin has
-- approved anything (admin_actions.actor_user_id) or a system account carries
-- ledger entries, these deletes hit their FKs and the Down fails. That is the
-- honest outcome — a down-migration must not shred a ledger to make itself pass.
-- TestMigrationsReversible runs on a pristine DB, which is why it stays green.
DELETE FROM users    WHERE username = 'admin';
DELETE FROM accounts WHERE system_code IN ('EXTERNAL_CLEARING', 'CASH', 'FEES');
