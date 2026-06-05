-- +goose Up
-- System (general-ledger) accounts are STRUCTURAL: money can only enter/leave the
-- bank through them, so they are seeded as part of the schema, not as demo data.
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
DELETE FROM users    WHERE username = 'admin';
DELETE FROM accounts WHERE system_code IN ('EXTERNAL_CLEARING', 'CASH', 'FEES');
