-- +goose Up
-- ─────────────────────────────────────────────────────────────────────────────
-- TRANSFERS & THE LEDGER — the double-entry source of truth and the money paths
-- The heart of bank0 (rules 1 & 2), split out of the former core_banking monolith.
-- Holds the transfers table (the operation/intent carrying the lifecycle), the
-- append-only ledger_entries source of truth, the idempotency_keys table, the
-- ledger triggers (the ONE balance writer + the append-only guard), and every money
-- function: request_transfer, post_transfer, cancel_transfer, the transfer()
-- auto-post, reverse_transfer (clawback), deposit/withdraw, and the client_*
-- ownership wrappers (+ assert_caller_owns).
--
-- Depends on accounts/holds (00004): the ledger trigger writes accounts.balance_minor
-- (under the in_ledger guard), and holds.transfer_id gets its FK to transfers here.
-- bank_settings / maker-checker (the request_money_with_approval staging path) live
-- in 00006; maintenance sweeps + reconcile() in 00007.
-- ─────────────────────────────────────────────────────────────────────────────

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
    -- Rail-ready identifiers (ISO 20022 / SWIFT). uetr is the bank-minted UUIDv4
    -- end-to-end trace id (stable across replays — minted once at insert);
    -- end_to_end_id is the optional ORIGINATOR-supplied reference (pain.001
    -- EndToEndId, <= 35 chars of the ISO restricted set). Neither affects money
    -- movement; they exist so the contract already speaks rail before a rail exists.
    uetr              UUID NOT NULL UNIQUE DEFAULT gen_random_uuid(),
    end_to_end_id     VARCHAR(35),
    failure_reason    TEXT,
    -- Hold lifecycle (Recs 22/25): why a transfer is parked (held/under_review) and
    -- when its confirmation/review window lapses. Both are set by place_transfer_hold
    -- and KEPT after release (posted/canceled) for audit — hence the one-way CHECK.
    hold_reason       TEXT,
    hold_expires_at   TIMESTAMPTZ,
    requested_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    posted_at         TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),

    CHECK (amount_minor > 0),
    CHECK (debit_account_id <> credit_account_id),
    CHECK (end_to_end_id IS NULL OR end_to_end_id ~ '^[A-Za-z0-9+?/:().,'' -]{1,35}$'),
    -- posted_at is set once a transfer reaches the ledger; a reversed transfer
    -- was posted, so it keeps its posted_at.
    CHECK ((posted_at IS NOT NULL) = (status IN ('posted', 'reversed'))),
    CHECK ((kind = 'reversal')  = (reverses_id IS NOT NULL)),
    -- one-way: a parked transfer MUST carry an expiry; released rows may keep it.
    CHECK (hold_expires_at IS NOT NULL OR status NOT IN ('held', 'under_review'))
);

-- Now that transfers exists, wire up holds.transfer_id (declared FK-less in 00004).
ALTER TABLE holds
    ADD CONSTRAINT holds_transfer_id_fkey FOREIGN KEY (transfer_id) REFERENCES transfers(id);

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
-- idempotency_keys  (DB-enforced replay safety for money moves)
-- ─────────────────────────────────────────────────────────────────────────────
-- owner_id namespaces the key to the owning principal (the customer user id on the
-- client money/account paths; the all-zero sentinel for system/anonymous/operator
-- ops). The PK is (owner_id, key) so one customer's key can never collide with, or
-- surface the stored result of, another's — replay/fingerprint semantics still hold
-- WITHIN a namespace, but the same raw key across two different owners is two
-- independent claims.
CREATE TABLE idempotency_keys (
    owner_id     UUID NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000',
    key          TEXT NOT NULL,
    scope        TEXT NOT NULL,
    request_hash TEXT NOT NULL,
    status       ik_status NOT NULL DEFAULT 'in_progress',
    transfer_id  UUID REFERENCES transfers(id),
    response     JSONB,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at   TIMESTAMPTZ NOT NULL DEFAULT now() + INTERVAL '7 days',
    PRIMARY KEY (owner_id, key)
);

-- ─────────────────────────────────────────────────────────────────────────────
-- indexes (statement pagination, pending queue, history, TTL, fuzzy search)
-- ─────────────────────────────────────────────────────────────────────────────
-- Account statement pagination (cursor on posted_at, id). id is UUIDv7 -> time-ordered tiebreak.
CREATE INDEX idx_ledger_account_posted ON ledger_entries (account_id, posted_at DESC, id DESC);
-- Fetch both legs of a transfer.
CREATE INDEX idx_ledger_transfer       ON ledger_entries (transfer_id);

-- operator "pending queue".
CREATE INDEX idx_transfers_pending     ON transfers (requested_at) WHERE status = 'pending';
-- per-account transfer history (the tf-backend UNION ALL pattern hits these independently).
CREATE INDEX idx_transfers_debit       ON transfers (debit_account_id, created_at DESC);
CREATE INDEX idx_transfers_credit      ON transfers (credit_account_id, created_at DESC);
-- ListMyTransfers keyset cursor on (requested_at, id) for each OR arm.
CREATE INDEX idx_transfers_debit_req   ON transfers (debit_account_id, requested_at DESC, id DESC);
CREATE INDEX idx_transfers_credit_req  ON transfers (credit_account_id, requested_at DESC, id DESC);
-- fuzzy transfer-description search (trigram GIN).
CREATE INDEX idx_transfers_desc_trgm   ON transfers USING gin (description gin_trgm_ops);
-- parked-transfer sweep + operator review queue (held/under_review by expiry).
CREATE INDEX idx_transfers_review      ON transfers (hold_expires_at) WHERE status IN ('held', 'under_review');
-- reversal lookup (reverse_transfer's already-reversed short-circuit, Rec 4).
CREATE INDEX idx_transfers_reverses    ON transfers (reverses_id) WHERE reverses_id IS NOT NULL;

-- idempotency key TTL cleanup.
CREATE INDEX idx_idempotency_expiry    ON idempotency_keys (expires_at);

-- +goose StatementBegin

-- ─────────────────────────────────────────────────────────────────────────────
-- Ledger triggers & trigger functions
-- ─────────────────────────────────────────────────────────────────────────────

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
        RAISE EXCEPTION 'ledger entry references missing account %', NEW.account_id
            USING ERRCODE = 'XX000';
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

-- Append-only enforcement: ledger history can never be edited or deleted.
-- Corrections happen via reverse_transfer (new inverse entries).
CREATE OR REPLACE FUNCTION ledger_block_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'ledger_entries is append-only (% blocked)', TG_OP
        USING ERRCODE = 'restrict_violation';
END;
$$ LANGUAGE plpgsql;

-- +goose StatementEnd

CREATE TRIGGER trg_transfers_updated_at BEFORE UPDATE ON transfers FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER trg_ledger_apply_balance BEFORE INSERT ON ledger_entries FOR EACH ROW EXECUTE FUNCTION ledger_apply_to_balance();
CREATE TRIGGER trg_ledger_immutable     BEFORE UPDATE OR DELETE ON ledger_entries FOR EACH ROW EXECUTE FUNCTION ledger_block_mutation();

-- +goose StatementBegin

-- ─────────────────────────────────────────────────────────────────────────────
-- request_transfer: create a pending transfer + an authorization hold.
-- Idempotent: a replayed key returns the original result and never double-posts.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION request_transfer(
    p_idempotency_key TEXT,
    p_debit_account   UUID,
    p_credit_account  UUID,
    p_amount_minor    BIGINT,
    p_description     TEXT          DEFAULT '',
    p_kind            transfer_kind DEFAULT 'transfer',
    p_hold_ttl        INTERVAL      DEFAULT INTERVAL '15 minutes',
    p_end_to_end_id   VARCHAR(35)   DEFAULT NULL,
    -- Owning principal for the idempotency namespace. The client path threads the
    -- authenticated subject (via client_transfer -> transfer); operator/system
    -- callers keep the all-zero sentinel and thus the old global semantics.
    p_caller          UUID          DEFAULT '00000000-0000-0000-0000-000000000000'
) RETURNS TABLE (transfer_id UUID, status transfer_status, was_replay BOOLEAN) AS $$
DECLARE
    v_hash      TEXT := encode(digest(
        coalesce(p_debit_account::text,'') || '|' ||
        coalesce(p_credit_account::text,'') || '|' ||
        p_amount_minor::text || '|' || p_kind::text || '|' ||
        coalesce(p_end_to_end_id,''), 'sha256'), 'hex');
    v_existing  idempotency_keys%ROWTYPE;
    v_debit     accounts%ROWTYPE;
    v_credit    accounts%ROWTYPE;
    v_available BIGINT;
    v_id        UUID;
BEGIN
    IF p_idempotency_key IS NULL OR p_idempotency_key = '' THEN
        RAISE EXCEPTION 'idempotency key is required' USING ERRCODE = 'check_violation';
    END IF;

    -- (a) Idempotency gate: first writer wins, within the caller's namespace.
    INSERT INTO idempotency_keys (owner_id, key, scope, request_hash, status)
    VALUES (p_caller, p_idempotency_key, 'transfer', v_hash, 'in_progress')
    ON CONFLICT (owner_id, key) DO NOTHING;

    IF NOT FOUND THEN
        SELECT * INTO v_existing FROM idempotency_keys
         WHERE owner_id = p_caller AND key = p_idempotency_key;
        IF v_existing.request_hash <> v_hash THEN
            RAISE EXCEPTION 'idempotency key % reused with different parameters', p_idempotency_key
                USING ERRCODE = 'check_violation';
        END IF;
        IF v_existing.status = 'in_progress' THEN
            RAISE EXCEPTION 'request for key % is still in progress', p_idempotency_key
                USING ERRCODE = 'object_in_use';
        END IF;
        RETURN QUERY
            SELECT t.id, t.status, TRUE FROM transfers t WHERE t.id = v_existing.transfer_id;
        RETURN;
    END IF;

    -- (b) Validate. Lock both accounts in a stable order to avoid deadlocks.
    IF p_debit_account = p_credit_account THEN
        RAISE EXCEPTION 'debit and credit accounts must differ';
    END IF;

    IF p_debit_account < p_credit_account THEN
        SELECT * INTO v_debit  FROM accounts WHERE id = p_debit_account  FOR UPDATE;
        SELECT * INTO v_credit FROM accounts WHERE id = p_credit_account FOR UPDATE;
    ELSE
        SELECT * INTO v_credit FROM accounts WHERE id = p_credit_account FOR UPDATE;
        SELECT * INTO v_debit  FROM accounts WHERE id = p_debit_account  FOR UPDATE;
    END IF;

    IF v_debit.id  IS NULL THEN RAISE EXCEPTION 'debit account % not found',  p_debit_account; END IF;
    IF v_credit.id IS NULL THEN RAISE EXCEPTION 'credit account % not found', p_credit_account; END IF;
    IF v_debit.status  <> 'active' THEN RAISE EXCEPTION 'debit account % not active',  p_debit_account; END IF;
    IF v_credit.status <> 'active' THEN RAISE EXCEPTION 'credit account % not active', p_credit_account; END IF;
    IF v_debit.currency <> v_credit.currency THEN RAISE EXCEPTION 'currency mismatch'; END IF;
    IF p_amount_minor <= 0 THEN RAISE EXCEPTION 'amount must be positive'; END IF;

    IF v_debit.kind = 'customer' THEN
        IF p_amount_minor > v_debit.transfer_limit_minor THEN
            RAISE EXCEPTION 'amount % exceeds transfer limit %', p_amount_minor, v_debit.transfer_limit_minor;
        END IF;

        SELECT v_debit.balance_minor - COALESCE(SUM(amount_minor), 0) INTO v_available
        FROM holds WHERE account_id = p_debit_account AND holds.status = 'active';

        IF v_available < p_amount_minor THEN
            RAISE EXCEPTION 'insufficient available funds: have %, need %', v_available, p_amount_minor
                USING ERRCODE = 'check_violation';
        END IF;
    END IF;

    -- (c) Create the pending transfer + the hold. uetr is minted by the column
    -- default (bank-minted, stable across replays since replays never reach here).
    INSERT INTO transfers (debit_account_id, credit_account_id, amount_minor,
                           currency, status, kind, description, idempotency_key, end_to_end_id)
    VALUES (p_debit_account, p_credit_account, p_amount_minor,
            v_debit.currency, 'pending', p_kind, p_description, p_idempotency_key,
            NULLIF(p_end_to_end_id, ''))
    RETURNING id INTO v_id;

    INSERT INTO holds (account_id, transfer_id, amount_minor, status, expires_at)
    VALUES (p_debit_account, v_id, p_amount_minor, 'active', now() + p_hold_ttl);

    -- (d) Record result against the idempotency key.
    UPDATE idempotency_keys
       SET status = 'completed', transfer_id = v_id,
           response = jsonb_build_object('transfer_id', v_id, 'status', 'pending')
     WHERE owner_id = p_caller AND key = p_idempotency_key;

    RETURN QUERY SELECT v_id, 'pending'::transfer_status, FALSE;
END;
$$ LANGUAGE plpgsql;

-- ─────────────────────────────────────────────────────────────────────────────
-- post_transfer: pending -> posted. Writes the two ledger legs (the balance
-- trigger does the rest), captures the hold. Idempotent on an already-posted row.
-- p_allow_from is the set of source states the caller may post FROM: it defaults to
-- {pending} so the plain admin/sqlc 1-arg call can never release a risk/screening
-- hold. The internal release paths pass the source state explicitly:
-- client_confirm_transfer -> {held}; approve_request (screening) -> {under_review}.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION post_transfer(
    p_transfer_id UUID,
    p_allow_from  transfer_status[] DEFAULT ARRAY['pending']::transfer_status[]
) RETURNS transfer_status AS $$
DECLARE
    v_t      transfers%ROWTYPE;
    v_debit  accounts%ROWTYPE;
    v_credit accounts%ROWTYPE;
BEGIN
    SELECT * INTO v_t FROM transfers WHERE id = p_transfer_id FOR UPDATE;
    IF NOT FOUND THEN RAISE EXCEPTION 'transfer % not found', p_transfer_id; END IF;

    IF v_t.status = 'posted' THEN RETURN 'posted'; END IF;          -- idempotent no-op
    IF NOT (v_t.status = ANY(p_allow_from)) THEN
        RAISE EXCEPTION 'cannot post transfer in state %', v_t.status USING ERRCODE = 'check_violation';
    END IF;

    INSERT INTO ledger_entries (transfer_id, account_id, direction, amount_minor, currency, balance_after)
    VALUES (p_transfer_id, v_t.debit_account_id,  'debit',  v_t.amount_minor, v_t.currency, 0),
           (p_transfer_id, v_t.credit_account_id, 'credit', v_t.amount_minor, v_t.currency, 0);
    -- balance_after is overwritten by the BEFORE INSERT trigger.

    UPDATE holds SET status = 'captured', released_at = now()
     WHERE transfer_id = p_transfer_id AND status = 'active';

    UPDATE transfers SET status = 'posted', posted_at = now() WHERE id = p_transfer_id;

    -- Notify both HUMAN parties in the same txn (events, 00008): the payer gets
    -- transfer.posted, the payee payment.incoming. System/GL sides (NULL user_id)
    -- emit nothing. Replays never reach here (idempotent no-op above), and the
    -- partial unique index absorbs any double emission. Counterparty is labelled
    -- by IBAN/system code only — never a name (no new disclosure).
    SELECT * INTO v_debit  FROM accounts WHERE id = v_t.debit_account_id;
    SELECT * INTO v_credit FROM accounts WHERE id = v_t.credit_account_id;
    IF v_debit.user_id IS NOT NULL THEN
        PERFORM emit_event(v_debit.user_id, 'transfer.posted',
            'Payment sent',
            'Your payment to ' || COALESCE(v_credit.iban, v_credit.system_code, 'account') || ' completed.',
            p_transfer_id, v_t.debit_account_id,
            jsonb_build_object('amount_minor', v_t.amount_minor, 'currency', v_t.currency,
                               'counterparty_iban', COALESCE(v_credit.iban, v_credit.system_code, ''),
                               'kind', v_t.kind));
    END IF;
    IF v_credit.user_id IS NOT NULL THEN
        PERFORM emit_event(v_credit.user_id, 'payment.incoming',
            'Payment received',
            'You received a payment from ' || COALESCE(v_debit.iban, v_debit.system_code, 'account') || '.',
            p_transfer_id, v_t.credit_account_id,
            jsonb_build_object('amount_minor', v_t.amount_minor, 'currency', v_t.currency,
                               'counterparty_iban', COALESCE(v_debit.iban, v_debit.system_code, ''),
                               'kind', v_t.kind));
    END IF;

    RETURN 'posted';
END;
$$ LANGUAGE plpgsql;

-- ─────────────────────────────────────────────────────────────────────────────
-- cancel_transfer: {pending, held, under_review} -> canceled, release the hold.
-- Held/under_review are cancellable too (customer withdraws a held payment; an
-- operator rejects a screening row; the sweep lapses either). (Hold-expiry failure
-- is written directly by expire_holds, 00007.)
-- ─────────────────────────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION cancel_transfer(p_transfer_id UUID, p_reason TEXT DEFAULT '')
RETURNS transfer_status AS $$
DECLARE v_status transfer_status;
BEGIN
    SELECT status INTO v_status FROM transfers WHERE id = p_transfer_id FOR UPDATE;
    IF NOT FOUND THEN RAISE EXCEPTION 'transfer % not found', p_transfer_id; END IF;
    IF v_status = 'canceled' THEN RETURN 'canceled'; END IF;        -- idempotent
    IF v_status NOT IN ('pending', 'held', 'under_review') THEN
        RAISE EXCEPTION 'cannot cancel transfer in state %', v_status USING ERRCODE = 'check_violation';
    END IF;

    UPDATE holds SET status = 'released', released_at = now()
     WHERE transfer_id = p_transfer_id AND status = 'active';
    UPDATE transfers SET status = 'canceled', failure_reason = NULLIF(p_reason, '')
     WHERE id = p_transfer_id;
    RETURN 'canceled';
END;
$$ LANGUAGE plpgsql;

-- ─────────────────────────────────────────────────────────────────────────────
-- place_transfer_hold: pending -> held | under_review. Parks a transfer for a
-- customer confirmation window (held) or operator screening/review (under_review)
-- WITHOUT touching the ledger — the authorization hold stays 'active' (funds still
-- reserved), only its expires_at is extended to cover the (longer) window. Because
-- no hold-status edge is crossed, the held_minor cache is untouched. Records an
-- admin_actions row (screening_hold = the operator queue entry for under_review;
-- risk_hold = audit-only for held) with the initiating customer as actor, and
-- notifies the payer via a transfer.held event. business_days is 1..4.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION place_transfer_hold(
    p_transfer_id   UUID,
    p_new_status    transfer_status,
    p_reason        TEXT,
    p_business_days INT,
    p_actor         UUID,
    p_detail        JSONB DEFAULT '{}'
) RETURNS transfer_status AS $$
DECLARE
    v_t       transfers%ROWTYPE;
    v_expires TIMESTAMPTZ;
    v_action  TEXT;
    v_debit   accounts%ROWTYPE;
BEGIN
    IF p_new_status NOT IN ('held', 'under_review') THEN
        RAISE EXCEPTION 'place_transfer_hold: % is not a hold state', p_new_status USING ERRCODE = 'XX000';
    END IF;
    IF p_business_days < 1 OR p_business_days > 4 THEN
        RAISE EXCEPTION 'place_transfer_hold: business days must be 1..4 (got %)', p_business_days USING ERRCODE = 'XX000';
    END IF;

    SELECT * INTO v_t FROM transfers WHERE id = p_transfer_id FOR UPDATE;
    IF NOT FOUND THEN RAISE EXCEPTION 'transfer % not found', p_transfer_id; END IF;
    IF v_t.status <> 'pending' THEN
        RAISE EXCEPTION 'cannot place a hold on a transfer in state %', v_t.status USING ERRCODE = 'check_violation';
    END IF;

    v_expires := add_business_days(now(), p_business_days);
    UPDATE transfers
       SET status = p_new_status, hold_reason = p_reason, hold_expires_at = v_expires
     WHERE id = p_transfer_id;
    -- Keep the funds reserved: extend the active hold to the window's end. No
    -- active<->non-active edge, so the held_minor cache stays correct untouched.
    UPDATE holds SET expires_at = v_expires
     WHERE transfer_id = p_transfer_id AND status = 'active';
    IF NOT FOUND THEN
        RAISE EXCEPTION 'no active hold for transfer %', p_transfer_id USING ERRCODE = 'XX000';
    END IF;

    v_action := CASE WHEN p_new_status = 'under_review' THEN 'screening_hold' ELSE 'risk_hold' END;
    INSERT INTO admin_actions (actor_user_id, action, target_id, detail)
    VALUES (p_actor, v_action, p_transfer_id,
            COALESCE(p_detail, '{}'::jsonb) ||
            jsonb_build_object('reason', p_reason, 'hold_status', p_new_status, 'hold_expires_at', v_expires));

    SELECT * INTO v_debit FROM accounts WHERE id = v_t.debit_account_id;
    IF v_debit.user_id IS NOT NULL THEN
        PERFORM emit_event(v_debit.user_id, 'transfer.held',
            'Payment held',
            CASE WHEN p_new_status = 'under_review'
                 THEN 'Your payment is being reviewed and will be released once checks complete.'
                 ELSE 'Your payment needs your confirmation before it can be sent.' END,
            p_transfer_id, v_t.debit_account_id,
            jsonb_build_object('hold_status', p_new_status, 'reason', p_reason,
                               'hold_expires_at', v_expires,
                               'amount_minor', v_t.amount_minor, 'currency', v_t.currency));
    END IF;

    RETURN p_new_status;
END;
$$ LANGUAGE plpgsql;


-- ─────────────────────────────────────────────────────────────────────────────
-- transfer: the auto-post convenience (request + post in one txn). Idempotent.
-- This is what POST /transfers calls by default.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION transfer(
    p_idempotency_key TEXT,
    p_debit_account   UUID,
    p_credit_account  UUID,
    p_amount_minor    BIGINT,
    p_description     TEXT          DEFAULT '',
    p_kind            transfer_kind DEFAULT 'transfer',
    p_end_to_end_id   VARCHAR(35)   DEFAULT NULL,
    -- Owning principal for the idempotency namespace; forwarded to request_transfer.
    -- Sentinel (system/operator) by default; client_transfer passes the subject.
    p_caller          UUID          DEFAULT '00000000-0000-0000-0000-000000000000'
) RETURNS TABLE (transfer_id UUID, status transfer_status, was_replay BOOLEAN) AS $$
DECLARE
    v_id       UUID;
    v_status   transfer_status;
    v_replay   BOOLEAN;
    v_sentinel UUID := '00000000-0000-0000-0000-000000000000';
    v_eval     RECORD;
    v_hit      RECORD;
BEGIN
    SELECT r.transfer_id, r.status, r.was_replay INTO v_id, v_status, v_replay
    FROM request_transfer(p_idempotency_key, p_debit_account, p_credit_account,
                          p_amount_minor, p_description, p_kind,
                          INTERVAL '15 minutes', p_end_to_end_id, p_caller) r;

    -- Replay short-circuits BEFORE any gate: a replayed key returns the live status
    -- verbatim (posted/held/under_review/…) and never re-runs screening/evaluation.
    IF v_replay OR v_status <> 'pending' THEN
        RETURN QUERY SELECT v_id, v_status, v_replay;
        RETURN;
    END IF;

    -- System/operator (sentinel) callers bypass the fraud/AML gates entirely:
    -- deposits, withdrawals, reversals, dispute reimbursement, maker-checker
    -- staging and seeds must post as before, never park behind a fraud gate.
    IF p_caller = v_sentinel THEN
        v_status := post_transfer(v_id);
        RETURN QUERY SELECT v_id, v_status, v_replay;
        RETURN;
    END IF;

    -- (1) AML screening gate (Rec 25): a watchlist hit on either party parks the
    -- payment for operator review (under_review) for 4 business days.
    SELECT * INTO v_hit FROM screen_payment(p_debit_account, p_credit_account);
    IF FOUND THEN
        v_status := place_transfer_hold(v_id, 'under_review', 'screening', 4, p_caller,
            jsonb_build_object('watchlist_entry_id', v_hit.entry_id,
                               'matched_name', v_hit.matched_name, 'side', v_hit.side));
        RETURN QUERY SELECT v_id, v_status, v_replay;
        RETURN;
    END IF;

    -- (2) Fraud/warning decision gate (Rec 22). exclude_transfer = v_id so the
    -- just-inserted pending row doesn't inflate its own velocity math (intent and
    -- submit agree). step_up axis is not enforced in the DB auto-post path (0).
    SELECT * INTO v_eval
    FROM evaluate_transfer(p_caller, p_debit_account, p_credit_account, p_amount_minor, 0, v_id);

    IF v_eval.decision = 'block' THEN
        RAISE EXCEPTION 'payment blocked: %', COALESCE(NULLIF(v_eval.headline, ''), 'this payment cannot be sent')
            USING ERRCODE = 'check_violation';
    END IF;

    IF v_eval.required_ack THEN
        PERFORM assert_warning_ack(p_caller, v_eval.category, p_debit_account,
                                   p_credit_account, p_amount_minor, v_eval.cooling_off_seconds);
    END IF;

    IF v_eval.decision = 'review' THEN
        v_status := place_transfer_hold(v_id, 'held',
                                        COALESCE(NULLIF(v_eval.category, ''), 'risk_warning'), 1, p_caller,
                                        jsonb_build_object('rule_id', v_eval.rule_id,
                                                           'reasons', to_jsonb(v_eval.reason_codes)));
    ELSE
        v_status := post_transfer(v_id);
    END IF;

    RETURN QUERY SELECT v_id, v_status, v_replay;
END;
$$ LANGUAGE plpgsql;

-- ─────────────────────────────────────────────────────────────────────────────
-- reverse_transfer (clawback safety): posted -> reversed. Appends inverse ledger
-- entries via a new 'reversal' transfer. The original is never edited. Idempotent
-- on the key. Verifies up front that the counterparty (the original CREDIT account,
-- which the reversal DEBITS) still holds enough to be clawed back — otherwise we'd
-- drive its balance below zero and trip the raw accounts CHECK. Reversal
-- deliberately does NOT enforce the active-account check the forward path applies;
-- a clawback is an operator remedy and may credit a frozen/closed account.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION reverse_transfer(
    p_transfer_id     UUID,
    p_idempotency_key TEXT,
    p_reason          TEXT
) RETURNS UUID AS $$
DECLARE
    v_orig     transfers%ROWTYPE;
    v_existing idempotency_keys%ROWTYPE;
    v_rev_id   UUID;
    v_cp       accounts%ROWTYPE;
    v_hash     TEXT := encode(digest('reverse|' || p_transfer_id::text, 'sha256'), 'hex');
BEGIN
    -- Operator clawback: keeps the all-zero sentinel namespace (global semantics).
    INSERT INTO idempotency_keys (owner_id, key, scope, request_hash, status)
    VALUES ('00000000-0000-0000-0000-000000000000', p_idempotency_key, 'reversal', v_hash, 'in_progress')
    ON CONFLICT (owner_id, key) DO NOTHING;

    IF NOT FOUND THEN
        SELECT * INTO v_existing FROM idempotency_keys
         WHERE owner_id = '00000000-0000-0000-0000-000000000000' AND key = p_idempotency_key;
        IF v_existing.request_hash <> v_hash THEN
            RAISE EXCEPTION 'idempotency key % reused with different parameters', p_idempotency_key
                USING ERRCODE = 'check_violation';
        END IF;
        RETURN v_existing.transfer_id;
    END IF;

    SELECT * INTO v_orig FROM transfers WHERE id = p_transfer_id FOR UPDATE;
    IF NOT FOUND THEN RAISE EXCEPTION 'transfer % not found', p_transfer_id; END IF;
    -- Rec 4: a second reverse (under a DIFFERENT key) is idempotent, not an error.
    -- Look up the existing reversal via reverses_id, complete THIS newly-claimed key
    -- pointing at it, and return its id — so every reverse of the same transfer,
    -- across any key, converges on one reversal.
    IF v_orig.status = 'reversed' THEN
        SELECT id INTO v_rev_id FROM transfers WHERE reverses_id = p_transfer_id LIMIT 1;
        IF v_rev_id IS NULL THEN
            RAISE EXCEPTION 'transfer % marked reversed but no reversal row exists', p_transfer_id
                USING ERRCODE = 'XX000';
        END IF;
        UPDATE idempotency_keys SET status = 'completed', transfer_id = v_rev_id
         WHERE owner_id = '00000000-0000-0000-0000-000000000000' AND key = p_idempotency_key;
        RETURN v_rev_id;
    END IF;
    IF v_orig.status <> 'posted' THEN
        RAISE EXCEPTION 'can only reverse a posted transfer (state %)', v_orig.status
            USING ERRCODE = 'check_violation';
    END IF;

    -- Clawback safety: lock the account the reversal will DEBIT (the original
    -- CREDIT account) and confirm it still holds enough to be reversed. Reversal
    -- intentionally bypasses the active-account check applied on the forward path
    -- (see the header note) — only the funds check is enforced here.
    SELECT * INTO v_cp FROM accounts WHERE id = v_orig.credit_account_id FOR UPDATE;
    IF v_cp.kind <> 'system' AND v_cp.balance_minor < v_orig.amount_minor THEN
        RAISE EXCEPTION 'cannot reverse transfer %: recipient has insufficient funds to claw back', p_transfer_id
            USING ERRCODE = 'check_violation';
    END IF;

    INSERT INTO transfers (debit_account_id, credit_account_id, amount_minor, currency,
                           status, kind, reverses_id, description, idempotency_key, posted_at)
    VALUES (v_orig.credit_account_id, v_orig.debit_account_id, v_orig.amount_minor, v_orig.currency,
            'posted', 'reversal', p_transfer_id, COALESCE(p_reason, ''), p_idempotency_key, now())
    RETURNING id INTO v_rev_id;

    INSERT INTO ledger_entries (transfer_id, account_id, direction, amount_minor, currency, balance_after)
    VALUES (v_rev_id, v_orig.credit_account_id, 'debit',  v_orig.amount_minor, v_orig.currency, 0),
           (v_rev_id, v_orig.debit_account_id,  'credit', v_orig.amount_minor, v_orig.currency, 0);

    UPDATE transfers SET status = 'reversed' WHERE id = p_transfer_id;
    UPDATE idempotency_keys SET status = 'completed', transfer_id = v_rev_id
     WHERE owner_id = '00000000-0000-0000-0000-000000000000' AND key = p_idempotency_key;
    RETURN v_rev_id;
END;
$$ LANGUAGE plpgsql;

-- ─────────────────────────────────────────────────────────────────────────────
-- deposit / withdraw: money crossing the bank boundary, via external_clearing.
-- A deposit is a posted transfer external_clearing -> customer (no money minted).
-- High-value moves that need 4-eyes go through request_money_with_approval (00006),
-- which stages a PENDING transfer directly via request_transfer.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION deposit(
    p_idempotency_key TEXT,
    p_account_id      UUID,
    p_amount_minor    BIGINT,
    p_description     TEXT DEFAULT 'Deposit'
) RETURNS UUID AS $$
DECLARE v_ext UUID; v_id UUID;
BEGIN
    SELECT id INTO v_ext FROM accounts WHERE system_code = 'EXTERNAL_CLEARING';
    IF NOT FOUND THEN RAISE EXCEPTION 'EXTERNAL_CLEARING system account missing (run seed)' USING ERRCODE = 'XX000'; END IF;
    SELECT t.transfer_id INTO v_id
    FROM transfer(p_idempotency_key, v_ext, p_account_id, p_amount_minor, p_description, 'deposit') t;
    RETURN v_id;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION withdraw(
    p_idempotency_key TEXT,
    p_account_id      UUID,
    p_amount_minor    BIGINT,
    p_description     TEXT DEFAULT 'Withdrawal'
) RETURNS UUID AS $$
DECLARE v_ext UUID; v_id UUID;
BEGIN
    SELECT id INTO v_ext FROM accounts WHERE system_code = 'EXTERNAL_CLEARING';
    IF NOT FOUND THEN RAISE EXCEPTION 'EXTERNAL_CLEARING system account missing (run seed)' USING ERRCODE = 'XX000'; END IF;
    SELECT t.transfer_id INTO v_id
    FROM transfer(p_idempotency_key, p_account_id, v_ext, p_amount_minor, p_description, 'withdrawal') t;
    RETURN v_id;
END;
$$ LANGUAGE plpgsql;

-- ─────────────────────────────────────────────────────────────────────────────
-- client_* ownership wrappers: client-surface money entrypoints that enforce caller
-- ownership IN THE DB, so the Go handlers skip a separate ownership-probe round trip.
-- The admin/console surface keeps calling the unguarded base functions.
-- ─────────────────────────────────────────────────────────────────────────────

-- assert_caller_owns: raise 42501 (-> 403) unless p_subject owns p_account. A
-- nonexistent or system-owned (NULL) account is treated as not-owned.
CREATE OR REPLACE FUNCTION assert_caller_owns(p_subject UUID, p_account UUID) RETURNS VOID AS $$
DECLARE v_owner UUID;
BEGIN
    SELECT user_id INTO v_owner FROM accounts WHERE id = p_account;
    IF v_owner IS NULL OR v_owner <> p_subject THEN
        RAISE EXCEPTION 'debit account not owned by caller' USING ERRCODE = '42501';
    END IF;
END;
$$ LANGUAGE plpgsql;

-- client_transfer: the caller may only debit an account they own; then auto-post via
-- transfer(). Same RETURNS shape as transfer() so the handler is unchanged downstream.
CREATE OR REPLACE FUNCTION client_transfer(
    p_caller_subject  UUID,
    p_idempotency_key TEXT,
    p_debit_account   UUID,
    p_credit_account  UUID,
    p_amount_minor    BIGINT,
    p_description     TEXT        DEFAULT '',
    p_end_to_end_id   VARCHAR(35) DEFAULT NULL
) RETURNS TABLE (transfer_id UUID, status transfer_status, was_replay BOOLEAN) AS $$
BEGIN
    PERFORM assert_caller_owns(p_caller_subject, p_debit_account);
    -- Namespace the idempotency key to the authenticated subject so one customer's
    -- key can never collide with another's (Rec 3).
    RETURN QUERY
        SELECT t.transfer_id, t.status, t.was_replay
        FROM transfer(p_idempotency_key, p_debit_account, p_credit_account,
                      p_amount_minor, p_description, 'transfer', p_end_to_end_id,
                      p_caller_subject) t;
END;
$$ LANGUAGE plpgsql;

-- client_post_transfer / client_cancel_transfer: act on a pending transfer only if
-- the caller owns its DEBIT account. A transfer the caller doesn't own (or that does
-- not exist) raises 'not found' -> 404, hiding existence.
CREATE OR REPLACE FUNCTION client_post_transfer(p_caller_subject UUID, p_transfer_id UUID)
RETURNS transfer_status AS $$
DECLARE v_owner UUID;
BEGIN
    SELECT a.user_id INTO v_owner
    FROM transfers t JOIN accounts a ON a.id = t.debit_account_id
    WHERE t.id = p_transfer_id;
    IF v_owner IS NULL OR v_owner <> p_caller_subject THEN
        RAISE EXCEPTION 'transfer % not found', p_transfer_id;
    END IF;
    RETURN post_transfer(p_transfer_id);
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION client_cancel_transfer(p_caller_subject UUID, p_transfer_id UUID, p_reason TEXT DEFAULT '')
RETURNS transfer_status AS $$
DECLARE v_owner UUID; v_status transfer_status;
BEGIN
    SELECT a.user_id, t.status INTO v_owner, v_status
    FROM transfers t JOIN accounts a ON a.id = t.debit_account_id
    WHERE t.id = p_transfer_id;
    IF v_owner IS NULL OR v_owner <> p_caller_subject THEN
        RAISE EXCEPTION 'transfer % not found', p_transfer_id;
    END IF;
    -- under_review is operator-only (screening/AML): the customer cannot cancel it.
    IF v_status = 'under_review' THEN
        RAISE EXCEPTION 'cannot cancel a transfer under review';   -- P0001 -> 409
    END IF;
    RETURN cancel_transfer(p_transfer_id, p_reason);
END;
$$ LANGUAGE plpgsql;

-- client_confirm_transfer: the customer releases their OWN held transfer (the Rec 22
-- cooling-off 'held' state). Ownership like client_post_transfer (a foreign/unknown
-- transfer -> 'not found' -> 404, hiding existence). Already-posted is an idempotent
-- no-op; a non-held state or a lapsed confirmation window -> P0001 'cannot …' (409).
-- Releases the hold via post_transfer(id, {held}); under_review is NOT confirmable
-- here (operator-only).
CREATE OR REPLACE FUNCTION client_confirm_transfer(p_caller_subject UUID, p_transfer_id UUID)
RETURNS transfer_status AS $$
DECLARE v_owner UUID; v_status transfer_status; v_expires TIMESTAMPTZ;
BEGIN
    SELECT a.user_id, t.status, t.hold_expires_at INTO v_owner, v_status, v_expires
    FROM transfers t JOIN accounts a ON a.id = t.debit_account_id
    WHERE t.id = p_transfer_id;
    IF v_owner IS NULL OR v_owner <> p_caller_subject THEN
        RAISE EXCEPTION 'transfer % not found', p_transfer_id;
    END IF;
    IF v_status = 'posted' THEN RETURN 'posted'; END IF;          -- idempotent
    IF v_status <> 'held' THEN
        RAISE EXCEPTION 'cannot confirm a transfer in state %', v_status;   -- P0001 -> 409
    END IF;
    IF v_expires IS NOT NULL AND v_expires < now() THEN
        RAISE EXCEPTION 'cannot confirm: the confirmation window has expired';   -- P0001 -> 409
    END IF;
    RETURN post_transfer(p_transfer_id, ARRAY['held']::transfer_status[]);
END;
$$ LANGUAGE plpgsql;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP FUNCTION IF EXISTS client_confirm_transfer(UUID, UUID);
DROP FUNCTION IF EXISTS client_cancel_transfer(UUID, UUID, TEXT);
DROP FUNCTION IF EXISTS client_post_transfer(UUID, UUID);
DROP FUNCTION IF EXISTS client_transfer(UUID, TEXT, UUID, UUID, BIGINT, TEXT, VARCHAR);
DROP FUNCTION IF EXISTS assert_caller_owns(UUID, UUID);
DROP FUNCTION IF EXISTS withdraw(TEXT, UUID, BIGINT, TEXT);
DROP FUNCTION IF EXISTS deposit(TEXT, UUID, BIGINT, TEXT);
DROP FUNCTION IF EXISTS reverse_transfer(UUID, TEXT, TEXT);
DROP FUNCTION IF EXISTS transfer(TEXT, UUID, UUID, BIGINT, TEXT, transfer_kind, VARCHAR, UUID);
DROP FUNCTION IF EXISTS place_transfer_hold(UUID, transfer_status, TEXT, INT, UUID, JSONB);
DROP FUNCTION IF EXISTS cancel_transfer(UUID, TEXT);
DROP FUNCTION IF EXISTS post_transfer(UUID, transfer_status[]);
DROP FUNCTION IF EXISTS request_transfer(TEXT, UUID, UUID, BIGINT, TEXT, transfer_kind, INTERVAL, VARCHAR, UUID);
-- +goose StatementEnd
DROP TRIGGER IF EXISTS trg_ledger_immutable     ON ledger_entries;
DROP TRIGGER IF EXISTS trg_ledger_apply_balance ON ledger_entries;
DROP TRIGGER IF EXISTS trg_transfers_updated_at ON transfers;
-- +goose StatementBegin
DROP FUNCTION IF EXISTS ledger_block_mutation();
DROP FUNCTION IF EXISTS ledger_apply_to_balance();
-- +goose StatementEnd
ALTER TABLE holds DROP CONSTRAINT IF EXISTS holds_transfer_id_fkey;
DROP TABLE IF EXISTS idempotency_keys;
DROP TABLE IF EXISTS ledger_entries;
DROP TABLE IF EXISTS transfers;
