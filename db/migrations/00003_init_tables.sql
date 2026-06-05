-- +goose Up

-- ─────────────────────────────────────────────────────────────────────────────
-- users
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE users (
    id            UUID PRIMARY KEY DEFAULT uuidv7(),
    username      CITEXT NOT NULL UNIQUE,
    password_hash TEXT   NOT NULL,
    full_name     TEXT   NOT NULL,
    email         CITEXT UNIQUE,
    phone_number  VARCHAR(16) UNIQUE,
    role          user_role   NOT NULL DEFAULT 'customer',
    status        user_status NOT NULL DEFAULT 'active',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (email IS NULL OR email ~* '^[^@\s]+@[^@\s]+\.[^@\s]{2,}$')
);

-- ─────────────────────────────────────────────────────────────────────────────
-- accounts  (balance_minor is a CACHE; only the ledger trigger may change it)
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

    CHECK (transfer_limit_minor >= 0),
    CHECK (currency = 'EUR'),                                            -- single currency, for now
    CHECK (iban IS NULL OR iban ~ '^[A-Z0-9]{15,34}$'),
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

-- ─────────────────────────────────────────────────────────────────────────────
-- transfers  (the operation/intent carrying the lifecycle; NOT the ledger)
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE transfers (
    id                UUID PRIMARY KEY DEFAULT uuidv7(),
    debit_account_id  UUID NOT NULL REFERENCES accounts(id) ON DELETE RESTRICT,
    credit_account_id UUID NOT NULL REFERENCES accounts(id) ON DELETE RESTRICT,
    amount_minor      BIGINT  NOT NULL,
    currency          CHAR(3) NOT NULL DEFAULT 'EUR',
    status            transfer_status NOT NULL DEFAULT 'pending',
    kind              transfer_kind   NOT NULL DEFAULT 'transfer',
    reverses_id       UUID REFERENCES transfers(id),
    description       TEXT NOT NULL DEFAULT '',
    idempotency_key   TEXT,                                              -- soft ref to idempotency_keys.key
    failure_reason    TEXT,
    requested_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    posted_at         TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),

    CHECK (amount_minor > 0),
    CHECK (debit_account_id <> credit_account_id),
    -- posted_at is set once a transfer reaches the ledger; a reversed transfer
    -- was posted, so it keeps its posted_at.
    CHECK ((posted_at IS NOT NULL) = (status IN ('posted', 'reversed'))),
    CHECK ((kind = 'reversal')  = (reverses_id IS NOT NULL))
);

-- ─────────────────────────────────────────────────────────────────────────────
-- ledger_entries  (append-only source of truth)
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE ledger_entries (
    id            UUID PRIMARY KEY DEFAULT uuidv7(),
    transfer_id   UUID NOT NULL REFERENCES transfers(id) ON DELETE RESTRICT,
    account_id    UUID NOT NULL REFERENCES accounts(id)  ON DELETE RESTRICT,
    direction     entry_direction NOT NULL,
    amount_minor  BIGINT NOT NULL,
    signed_amount BIGINT GENERATED ALWAYS AS
                  (CASE direction WHEN 'debit' THEN -amount_minor ELSE amount_minor END) STORED,
    balance_after BIGINT NOT NULL,                                       -- running balance, set by trigger
    currency      CHAR(3) NOT NULL DEFAULT 'EUR',
    posted_at     TIMESTAMPTZ NOT NULL DEFAULT now(),

    CHECK (amount_minor > 0)
);

-- ─────────────────────────────────────────────────────────────────────────────
-- holds  (authorization reservations: available = balance - SUM(active holds))
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE holds (
    id           UUID PRIMARY KEY DEFAULT uuidv7(),
    account_id   UUID NOT NULL REFERENCES accounts(id) ON DELETE RESTRICT,
    transfer_id  UUID REFERENCES transfers(id),
    amount_minor BIGINT NOT NULL,
    status       hold_status NOT NULL DEFAULT 'active',
    expires_at   TIMESTAMPTZ NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    released_at  TIMESTAMPTZ,
    CHECK (amount_minor > 0)
);

CREATE UNIQUE INDEX uq_holds_active_transfer ON holds (transfer_id) WHERE status = 'active';

-- ─────────────────────────────────────────────────────────────────────────────
-- idempotency_keys  (DB-enforced replay safety for money moves)
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE idempotency_keys (
    key          TEXT PRIMARY KEY,
    scope        TEXT NOT NULL,
    request_hash TEXT NOT NULL,
    status       ik_status NOT NULL DEFAULT 'in_progress',
    transfer_id  UUID REFERENCES transfers(id),
    response     JSONB,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at   TIMESTAMPTZ NOT NULL DEFAULT now() + INTERVAL '7 days'
);

-- ─────────────────────────────────────────────────────────────────────────────
-- admin_actions  (operator audit; the "who authorized it & why" alongside the ledger)
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE admin_actions (
    id            UUID PRIMARY KEY DEFAULT uuidv7(),
    actor_user_id UUID NOT NULL REFERENCES users(id),
    action        TEXT NOT NULL,
    target_id     UUID,
    detail        JSONB NOT NULL DEFAULT '{}',
    approved_by   UUID REFERENCES users(id),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE IF EXISTS admin_actions;
DROP TABLE IF EXISTS idempotency_keys;
DROP TABLE IF EXISTS holds;
DROP TABLE IF EXISTS ledger_entries;
DROP TABLE IF EXISTS transfers;
DROP TABLE IF EXISTS accounts;
DROP TABLE IF EXISTS users;
