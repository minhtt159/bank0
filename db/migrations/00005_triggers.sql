-- +goose Up
-- +goose StatementBegin

-- updated_at maintenance on mutable tables.
CREATE OR REPLACE FUNCTION set_updated_at() RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at := now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- The ONE balance writer. Inserting a ledger entry is the only thing in the
-- entire system that may change accounts.balance_minor. It also stamps the
-- per-entry running balance (balance_after).
--
-- Runs BEFORE INSERT so it can set NEW.balance_after. We compute the signed
-- delta directly from direction/amount (the generated column signed_amount is
-- not yet populated in a BEFORE trigger).
CREATE OR REPLACE FUNCTION ledger_apply_to_balance() RETURNS TRIGGER AS $$
DECLARE
    v_delta   BIGINT := CASE NEW.direction WHEN 'debit' THEN -NEW.amount_minor
                                            ELSE NEW.amount_minor END;
    v_balance BIGINT;
BEGIN
    SELECT balance_minor INTO v_balance FROM accounts WHERE id = NEW.account_id FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'ledger entry references missing account %', NEW.account_id;
    END IF;

    v_balance := v_balance + v_delta;
    NEW.balance_after := v_balance;

    -- Flag this UPDATE as ledger-originated so the balance guard allows it.
    PERFORM set_config('bank0.in_ledger', 'on', true);
    UPDATE accounts SET balance_minor = v_balance WHERE id = NEW.account_id;
    PERFORM set_config('bank0.in_ledger', 'off', true);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- Tamper guard: balance_minor may change ONLY via the ledger trigger above.
-- Any other UPDATE (admin mistake, stray psql, buggy function) is rejected, so
-- the cache can never silently diverge from the ledger.
CREATE OR REPLACE FUNCTION account_guard_balance() RETURNS TRIGGER AS $$
BEGIN
    IF NEW.balance_minor IS DISTINCT FROM OLD.balance_minor
       AND current_setting('bank0.in_ledger', true) IS DISTINCT FROM 'on' THEN
        RAISE EXCEPTION 'balance_minor can only change via ledger entries (account %)', NEW.id
            USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- Append-only enforcement: ledger history can never be edited or deleted.
-- Corrections happen via reverse_transfer (new inverse entries).
CREATE OR REPLACE FUNCTION ledger_block_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'ledger_entries is append-only (% blocked)', TG_OP
        USING ERRCODE = 'restrict_violation';
END;
$$ LANGUAGE plpgsql;

-- +goose StatementEnd

CREATE TRIGGER trg_users_updated_at     BEFORE UPDATE ON users     FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER trg_accounts_updated_at  BEFORE UPDATE ON accounts  FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER trg_accounts_guard_balance BEFORE UPDATE ON accounts FOR EACH ROW EXECUTE FUNCTION account_guard_balance();
CREATE TRIGGER trg_transfers_updated_at BEFORE UPDATE ON transfers FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TRIGGER trg_ledger_apply_balance BEFORE INSERT ON ledger_entries FOR EACH ROW EXECUTE FUNCTION ledger_apply_to_balance();
CREATE TRIGGER trg_ledger_immutable     BEFORE UPDATE OR DELETE ON ledger_entries FOR EACH ROW EXECUTE FUNCTION ledger_block_mutation();

-- +goose Down
DROP TRIGGER IF EXISTS trg_ledger_immutable       ON ledger_entries;
DROP TRIGGER IF EXISTS trg_ledger_apply_balance   ON ledger_entries;
DROP TRIGGER IF EXISTS trg_transfers_updated_at   ON transfers;
DROP TRIGGER IF EXISTS trg_accounts_guard_balance ON accounts;
DROP TRIGGER IF EXISTS trg_accounts_updated_at    ON accounts;
DROP TRIGGER IF EXISTS trg_users_updated_at       ON users;
DROP FUNCTION IF EXISTS account_guard_balance();
DROP FUNCTION IF EXISTS ledger_block_mutation();
DROP FUNCTION IF EXISTS ledger_apply_to_balance();
DROP FUNCTION IF EXISTS set_updated_at();
