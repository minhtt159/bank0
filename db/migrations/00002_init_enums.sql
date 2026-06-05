-- +goose Up
CREATE TYPE user_role       AS ENUM ('customer', 'operator', 'admin', 'auditor');
CREATE TYPE user_status     AS ENUM ('active', 'locked', 'closed');

CREATE TYPE account_kind    AS ENUM ('customer', 'system');
CREATE TYPE account_status  AS ENUM ('active', 'frozen', 'closed');

CREATE TYPE transfer_status AS ENUM ('pending', 'posted', 'failed', 'canceled', 'reversed');
CREATE TYPE transfer_kind   AS ENUM ('transfer', 'deposit', 'withdrawal', 'reversal', 'fee', 'adjustment');

CREATE TYPE entry_direction AS ENUM ('debit', 'credit');
CREATE TYPE hold_status     AS ENUM ('active', 'captured', 'released', 'expired');
CREATE TYPE ik_status       AS ENUM ('in_progress', 'completed');

-- +goose Down
DROP TYPE IF EXISTS ik_status;
DROP TYPE IF EXISTS hold_status;
DROP TYPE IF EXISTS entry_direction;
DROP TYPE IF EXISTS transfer_kind;
DROP TYPE IF EXISTS transfer_status;
DROP TYPE IF EXISTS account_status;
DROP TYPE IF EXISTS account_kind;
DROP TYPE IF EXISTS user_status;
DROP TYPE IF EXISTS user_role;
