-- +goose Up
-- ─────────────────────────────────────────────────────────────────────────────
-- ACCOUNTS & HOLDS — the account model and authorization reservations
-- The first slice of the former core_banking monolith (split for 1.0). Holds the
-- accounts table (with a trigger-maintained balance_minor CACHE and a held_minor
-- mirror), the holds table (authorization reservations — account_available reads
-- SUM(active holds), so the two live together), the balance tamper-guard +
-- held-cache trigger fns, and the account functions
-- (create / available / set-default / set-status / update-limit).
--
-- accounts.iban's checksum CHECK calls iban_is_valid() from the IBAN file (00002);
-- the shared set_updated_at() trigger fn lives in the users file (00003), and
-- trg_accounts_updated_at hangs on it. create_account() sources its default limit
-- from default_transfer_limit() (bank_settings, 00009) — a runtime call inside a
-- plpgsql body, so it loads fine ahead of that file. The transfers table (00008)
-- back-references holds via an FK added there.
-- ─────────────────────────────────────────────────────────────────────────────

-- ─────────────────────────────────────────────────────────────────────────────
-- accounts  (balance_minor is a CACHE; only the ledger trigger may change it.
--            held_minor mirrors SUM(active holds); the holds trigger maintains it.)
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE accounts (
    id                   UUID PRIMARY KEY DEFAULT uuidv7(),
    user_id              UUID REFERENCES users(id) ON DELETE RESTRICT,   -- NULL for system
    kind                 account_kind   NOT NULL DEFAULT 'customer',
    system_code          TEXT UNIQUE,                                    -- e.g. EXTERNAL_CLEARING (system only)
    iban                 VARCHAR(34) UNIQUE,                             -- NULL for system accounts
    pin_hash             TEXT,                                           -- bcrypt; customer only
    currency             CHAR(3) NOT NULL DEFAULT 'EUR',
    balance_minor        BIGINT  NOT NULL DEFAULT 0,                     -- CACHE, trigger-maintained
    transfer_limit_minor BIGINT  NOT NULL DEFAULT 50000,                 -- €500.00
    is_default           BOOLEAN NOT NULL DEFAULT FALSE,
    status               account_status NOT NULL DEFAULT 'active',
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- held_minor mirrors SUM(active holds), maintained by the holds trigger. Kept
    -- as the trailing column to match the historical physical order (it was added
    -- by ALTER in the pre-split schema, so generated structs order it last).
    held_minor           BIGINT  NOT NULL DEFAULT 0 CHECK (held_minor >= 0),

    CHECK (transfer_limit_minor >= 0),
    CHECK (currency = 'EUR'),                                            -- single currency, for now
    -- The non-bypassable IBAN backstop: every customer IBAN must pass the checksum
    -- authority (iban_is_valid, 00002 — length + charset + MOD-97, strictly stronger
    -- than a bare format regex, which is why no separate format CHECK exists).
    -- System/GL accounts have iban NULL, which is allowed.
    CONSTRAINT accounts_iban_checksum CHECK (iban IS NULL OR iban_is_valid(iban)),
    -- customers cannot go negative; system (GL) accounts can:
    CHECK (kind = 'system' OR balance_minor >= 0),
    -- system accounts have a code and no owner/iban; customers have owner+iban and no code:
    CHECK (
        (kind = 'system'   AND system_code IS NOT NULL AND user_id IS NULL AND iban IS NULL)
     OR (kind = 'customer' AND system_code IS NULL     AND user_id IS NOT NULL AND iban IS NOT NULL)
    )
);

-- exactly one default account per user
CREATE UNIQUE INDEX uq_accounts_one_default ON accounts (user_id) WHERE is_default;
-- accounts.user_id: ListAccountsByUser, /me lookups, set_default_account/create_account.
CREATE INDEX idx_accounts_user ON accounts (user_id);
-- fuzzy IBAN search (trigram GIN).
CREATE INDEX idx_accounts_iban_trgm ON accounts USING gin ((iban::text) gin_trgm_ops);

-- ─────────────────────────────────────────────────────────────────────────────
-- holds  (authorization reservations: available = balance - SUM(active holds))
-- The transfer_id FK is added in 00008 once the transfers table exists.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE holds (
    id           UUID PRIMARY KEY DEFAULT uuidv7(),
    account_id   UUID NOT NULL REFERENCES accounts(id) ON DELETE RESTRICT,
    transfer_id  UUID,                                                   -- FK -> transfers added in 00008
    amount_minor BIGINT NOT NULL,
    status       hold_status NOT NULL DEFAULT 'active',
    expires_at   TIMESTAMPTZ NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    released_at  TIMESTAMPTZ,
    CHECK (amount_minor > 0)
);

CREATE UNIQUE INDEX uq_holds_active_transfer ON holds (transfer_id) WHERE status = 'active';
-- available-balance computation and expiry sweep (partial: active holds only).
CREATE INDEX idx_holds_active_account ON holds (account_id) WHERE status = 'active';
CREATE INDEX idx_holds_expiry         ON holds (expires_at) WHERE status = 'active';

-- +goose StatementBegin

-- ─────────────────────────────────────────────────────────────────────────────
-- Account trigger functions  (the shared set_updated_at() lives in 00003)
-- ─────────────────────────────────────────────────────────────────────────────

-- Tamper guard: balance_minor may change ONLY via the ledger trigger (00008). Any
-- other UPDATE (admin mistake, stray psql, buggy function) is rejected, so the
-- cache can never silently diverge from the ledger.
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

-- +goose StatementEnd

-- Attach the account/holds triggers. trg_accounts_updated_at uses the shared
-- set_updated_at() defined in the users file (00003). The balance guard protects
-- the cache; the held-cache trigger maintains held_minor.
CREATE TRIGGER trg_accounts_updated_at    BEFORE UPDATE ON accounts FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER trg_accounts_guard_balance BEFORE UPDATE ON accounts FOR EACH ROW EXECUTE FUNCTION account_guard_balance();

CREATE TRIGGER trg_holds_maintain_held
    AFTER INSERT OR UPDATE OR DELETE ON holds
    FOR EACH ROW EXECUTE FUNCTION holds_maintain_held();

-- +goose StatementBegin

-- ─────────────────────────────────────────────────────────────────────────────
-- Account functions
-- ─────────────────────────────────────────────────────────────────────────────

-- account_available = balance - held_minor (an O(1) two-column read; held_minor is
-- the trigger-maintained SUM(active holds)). The funds figure the account lists and
-- the operator console rely on for DISPLAY. (The money-authorizing funds check in
-- request_transfer recomputes a fresh SUM — a decision must not trust a cache.)
CREATE OR REPLACE FUNCTION account_available(p_account_id UUID) RETURNS BIGINT AS $$
    SELECT balance_minor - held_minor FROM accounts WHERE id = p_account_id;
$$ LANGUAGE sql STABLE;

-- create_account: first account for a user becomes the default. The partial unique
-- index (uq_accounts_one_default) guarantees there is never more than one. Sources
-- its default limit from bank_settings (default_transfer_limit, 00009) when the
-- caller passes <= 0.
CREATE OR REPLACE FUNCTION create_account(
    p_user_id              UUID,
    p_iban                 VARCHAR(34),
    p_pin                  TEXT,
    p_transfer_limit_minor BIGINT DEFAULT 0
) RETURNS UUID AS $$
DECLARE
    v_account_id UUID;
    v_is_default BOOLEAN;
BEGIN
    PERFORM 1 FROM users WHERE id = p_user_id FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'user % does not exist', p_user_id;
    END IF;

    IF p_transfer_limit_minor <= 0 THEN
        p_transfer_limit_minor := default_transfer_limit();
    END IF;

    v_is_default := NOT EXISTS (
        SELECT 1 FROM accounts WHERE user_id = p_user_id AND is_default
    );

    INSERT INTO accounts (user_id, kind, iban, pin_hash, transfer_limit_minor, is_default)
    VALUES (p_user_id, 'customer', p_iban,
            crypt(p_pin, gen_salt('bf', 10)), p_transfer_limit_minor, v_is_default)
    RETURNING id INTO v_account_id;

    RETURN v_account_id;
END;
$$ LANGUAGE plpgsql;

-- ─────────────────────────────────────────────────────────────────────────────
-- Customer self-service account opening (spec-customer-account-opening).
-- The server mints the IBAN via allocate_iban() — bank0's own minting POLICY,
-- deliberately not part of this generic accounts domain; it lives in
-- 00017_iban_minting.sql and binds at CALL time (plpgsql), the same later-object
-- pattern open_customer_account uses for idempotency_keys (00008).
-- ─────────────────────────────────────────────────────────────────────────────

-- open_customer_account: a customer opens an account for THEMSELVES — the server
-- allocates the IBAN, the limit comes from bank_settings (create_account sources
-- default_transfer_limit() on limit<=0), and the per-user cap comes from
-- bank_settings.max_accounts_per_user (bank policy, operator-tunable). The PIN
-- exists only to satisfy create_account (card/ATM path is out of scope for the
-- client surface): crypto-strong random, never returned, never logged.
-- Idempotent via the same claim-key gate as request_transfer (scope
-- 'open_account'): a mobile retry returns the originally-opened account instead
-- of opening a second.
CREATE OR REPLACE FUNCTION open_customer_account(
    p_idempotency_key TEXT,
    p_user_id         UUID
) RETURNS TABLE (account_id UUID, was_replay BOOLEAN) AS $$
DECLARE
    -- scalar vars, not idempotency_keys%ROWTYPE: %ROWTYPE resolves at CREATE
    -- time and the table lives in 00008 (this function only runs after both exist).
    v_hash      TEXT := encode(digest('open_account|' || p_user_id::text, 'sha256'), 'hex');
    v_ex_scope  TEXT;
    v_ex_hash   TEXT;
    v_ex_status ik_status;
    v_ex_resp   JSONB;
    v_count INT; v_cap INT; v_pin TEXT; v_id UUID;
BEGIN
    IF p_idempotency_key IS NULL OR p_idempotency_key = '' THEN
        RAISE EXCEPTION 'idempotency key is required' USING ERRCODE = 'check_violation';
    END IF;

    -- Namespace the claim to the opening user (Rec 3): the customer owns the key.
    INSERT INTO idempotency_keys (owner_id, key, scope, request_hash, status)
    VALUES (p_user_id, p_idempotency_key, 'open_account', v_hash, 'in_progress')
    ON CONFLICT (owner_id, key) DO NOTHING;

    IF NOT FOUND THEN
        SELECT ik.scope, ik.request_hash, ik.status, ik.response
          INTO v_ex_scope, v_ex_hash, v_ex_status, v_ex_resp
          FROM idempotency_keys ik WHERE ik.owner_id = p_user_id AND ik.key = p_idempotency_key;
        IF v_ex_scope <> 'open_account' OR v_ex_hash <> v_hash THEN
            RAISE EXCEPTION 'idempotency key reused with different parameters'
                USING ERRCODE = 'check_violation';
        END IF;
        IF v_ex_status = 'in_progress' THEN
            RAISE EXCEPTION 'request with this idempotency key is in progress'
                USING ERRCODE = 'object_in_use';
        END IF;
        RETURN QUERY SELECT (v_ex_resp->>'account_id')::uuid, TRUE;
        RETURN;
    END IF;

    PERFORM 1 FROM users WHERE id = p_user_id AND status = 'active' FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'user % not found or not active', p_user_id;
    END IF;

    -- the user row is locked above, so two concurrent self-opens serialize and
    -- the cap cannot be raced past.
    v_cap := max_accounts_per_user();
    SELECT count(*) INTO v_count FROM accounts
     WHERE user_id = p_user_id AND status <> 'closed';
    IF v_count >= v_cap THEN
        RAISE EXCEPTION 'account limit reached (% of %)', v_count, v_cap
            USING ERRCODE = 'check_violation';   -- handler maps the cap -> 409
    END IF;

    v_pin := lpad(((('x' || encode(gen_random_bytes(4), 'hex'))::bit(32)::bigint & 2147483647) % 1000000)::text, 6, '0');
    v_id  := create_account(p_user_id, allocate_iban(), v_pin, 0);

    UPDATE idempotency_keys
       SET status = 'completed', response = jsonb_build_object('account_id', v_id)
     WHERE owner_id = p_user_id AND key = p_idempotency_key;

    RETURN QUERY SELECT v_id, FALSE;
END;
$$ LANGUAGE plpgsql;

-- set_default_account: clear-then-set in one statement-pair under a user lock.
CREATE OR REPLACE FUNCTION set_default_account(
    p_user_id    UUID,
    p_account_id UUID
) RETURNS VOID AS $$
BEGIN
    PERFORM 1 FROM users WHERE id = p_user_id FOR UPDATE;
    IF NOT EXISTS (SELECT 1 FROM accounts WHERE id = p_account_id AND user_id = p_user_id) THEN
        RAISE EXCEPTION 'account % does not belong to user %', p_account_id, p_user_id;
    END IF;

    UPDATE accounts SET is_default = FALSE WHERE user_id = p_user_id AND is_default;
    UPDATE accounts SET is_default = TRUE  WHERE id = p_account_id;
END;
$$ LANGUAGE plpgsql;

-- set_account_status: active | frozen | closed. A frozen/closed account is
-- rejected by request_transfer (status <> 'active').
CREATE OR REPLACE FUNCTION set_account_status(
    p_account_id UUID,
    p_status     account_status
) RETURNS VOID AS $$
BEGIN
    UPDATE accounts SET status = p_status WHERE id = p_account_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'account % does not exist', p_account_id;
    END IF;
END;
$$ LANGUAGE plpgsql;

-- update_transfer_limit. NOTE: there is deliberately NO function to set balance
-- directly — balance changes only via the ledger (see deposit/withdraw/transfer).
CREATE OR REPLACE FUNCTION update_transfer_limit(
    p_account_id           UUID,
    p_transfer_limit_minor BIGINT
) RETURNS VOID AS $$
BEGIN
    IF p_transfer_limit_minor < 0 THEN
        RAISE EXCEPTION 'transfer limit must be >= 0';
    END IF;
    UPDATE accounts SET transfer_limit_minor = p_transfer_limit_minor WHERE id = p_account_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'account % does not exist', p_account_id;
    END IF;
END;
$$ LANGUAGE plpgsql;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP FUNCTION IF EXISTS update_transfer_limit(UUID, BIGINT);
DROP FUNCTION IF EXISTS set_account_status(UUID, account_status);
DROP FUNCTION IF EXISTS set_default_account(UUID, UUID);
DROP FUNCTION IF EXISTS open_customer_account(TEXT, UUID);
DROP FUNCTION IF EXISTS create_account(UUID, VARCHAR, TEXT, BIGINT);
DROP FUNCTION IF EXISTS account_available(UUID);
-- +goose StatementEnd
DROP TRIGGER IF EXISTS trg_holds_maintain_held    ON holds;
DROP TRIGGER IF EXISTS trg_accounts_guard_balance ON accounts;
DROP TRIGGER IF EXISTS trg_accounts_updated_at    ON accounts;
-- +goose StatementBegin
DROP FUNCTION IF EXISTS holds_maintain_held();
DROP FUNCTION IF EXISTS account_guard_balance();
-- +goose StatementEnd
DROP TABLE IF EXISTS holds;
DROP TABLE IF EXISTS accounts;
