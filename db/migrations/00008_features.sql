-- +goose Up
-- ─────────────────────────────────────────────────────────────────────────────
-- AUXILIARY FEATURES — payees, guided transfers & disputes
-- Customer-facing extras layered on top of core banking, none of which hold money
-- state: saved beneficiaries (with confirmation-of-payee), the guided-transfer
-- "mule menu" demo (fraudbank's APP-scam simulation), and the dispute case workflow.
-- Their functions lean on the IBAN primitives (00002), the masking helper, and the
-- core tables/functions (accounts 00004, transfers 00005, admin_actions 00006). The
-- dispute taxonomy enums live in 00001; the shared set_updated_at() trigger fn
-- (00004) backs trg_disputes_updated_at.
-- ─────────────────────────────────────────────────────────────────────────────

-- ─────────────────────────────────────────────────────────────────────────────
-- beneficiaries  (saved payees for the customer web app, docs/07)
-- A directory entry the customer can fuzzy-search and transfer to; carries the
-- resolved destination account id so createTransfer is unchanged. No money state —
-- ownership is always scoped to owner_user_id (the JWT subject).
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE beneficiaries (
    id                 UUID PRIMARY KEY DEFAULT uuidv7(),
    owner_user_id      UUID NOT NULL REFERENCES users(id)    ON DELETE CASCADE,
    label              TEXT NOT NULL,
    credit_account_id  UUID NOT NULL REFERENCES accounts(id) ON DELETE RESTRICT,
    iban               VARCHAR(34) NOT NULL,
    owner_name_masked  TEXT NOT NULL DEFAULT '',
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (label <> ''),
    UNIQUE (owner_user_id, credit_account_id)
);
CREATE INDEX idx_beneficiaries_owner ON beneficiaries (owner_user_id);

-- Every persisted IBAN passes the same checksum authority as accounts (00002/00004 accounts).
ALTER TABLE beneficiaries
    ADD CONSTRAINT beneficiaries_iban_checksum CHECK (iban_is_valid(iban));

-- ─────────────────────────────────────────────────────────────────────────────
-- guided_scenarios  (demo/config for fraudbank's "Guided transaction" APP-scam mode)
-- Maps an active demo to a target ("mule") account that GET /transfers/suggestion
-- will short-list. NO money state. Operator/seed-controlled; the client only toggles
-- whether it ASKS for a suggestion, never which account is returned.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE guided_scenarios (
    id                UUID PRIMARY KEY DEFAULT uuidv7(),
    name              TEXT NOT NULL UNIQUE,
    target_account_id UUID NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    reason            TEXT NOT NULL DEFAULT 'Recommended payee',
    active            BOOLEAN NOT NULL DEFAULT TRUE,
    target_user_id    UUID REFERENCES users(id) ON DELETE CASCADE,   -- NULL = any caller
    min_amount_minor  BIGINT NOT NULL DEFAULT 0,
    priority          INT NOT NULL DEFAULT 0,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (name <> ''),
    CHECK (min_amount_minor >= 0)
);
CREATE INDEX idx_guided_scenarios_active ON guided_scenarios (priority DESC) WHERE active;

-- ─────────────────────────────────────────────────────────────────────────────
-- disputes  (a customer "I don't recognise this" case against a transfer)
-- NOT money state — the ledger is append-only; remedy stays operator-side
-- (reverse_transfer). Only this row's status/resolution fields mutate.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE disputes (
    id                UUID PRIMARY KEY DEFAULT uuidv7(),
    transfer_id       UUID NOT NULL REFERENCES transfers(id) ON DELETE RESTRICT,
    raised_by_user_id UUID NOT NULL REFERENCES users(id)     ON DELETE RESTRICT,
    status            dispute_status   NOT NULL DEFAULT 'open',
    category          dispute_category NOT NULL DEFAULT 'unrecognised',
    reason            TEXT NOT NULL DEFAULT '',
    resolver_user_id  UUID REFERENCES users(id),
    resolution_note   TEXT NOT NULL DEFAULT '',
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- at most one non-terminal dispute per (transfer, raiser) -> 23505 -> 409
CREATE UNIQUE INDEX uq_disputes_one_open
  ON disputes (transfer_id, raised_by_user_id)
  WHERE status IN ('open', 'under_review');

CREATE INDEX idx_disputes_raiser ON disputes (raised_by_user_id, created_at DESC);
CREATE INDEX idx_disputes_queue  ON disputes (created_at DESC) WHERE status IN ('open', 'under_review');

-- updated_at maintenance: reuse the project's shared trigger fn from core banking.
CREATE TRIGGER trg_disputes_updated_at
  BEFORE UPDATE ON disputes
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- +goose StatementBegin

-- ─────────────────────────────────────────────────────────────────────────────
-- Beneficiary / confirmation-of-payee functions
-- ─────────────────────────────────────────────────────────────────────────────

-- resolve_account_by_iban: confirmation-of-payee. Returns the destination account
-- id + a MASKED owner name for an active customer account. Never exposes the balance
-- or the full name. Raises (-> 404) if not found / inactive.
CREATE OR REPLACE FUNCTION resolve_account_by_iban(p_iban VARCHAR)
RETURNS TABLE (account_id UUID, iban VARCHAR, owner_name_masked TEXT) AS $$
BEGIN
    RETURN QUERY
    SELECT a.id, a.iban, mask_name(u.full_name)
    FROM accounts a
    JOIN users u ON u.id = a.user_id
    WHERE a.iban = p_iban AND a.kind = 'customer' AND a.status = 'active';
    IF NOT FOUND THEN
        RAISE EXCEPTION 'account with iban % not found', p_iban;
    END IF;
END;
$$ LANGUAGE plpgsql STABLE;

-- add_beneficiary: resolve the IBAN, then store the entry for p_owner. Rejects
-- saving your own account. Duplicate (owner, account) hits the UNIQUE index
-- (23505 -> 409).
CREATE OR REPLACE FUNCTION add_beneficiary(
    p_owner UUID,
    p_label TEXT,
    p_iban  VARCHAR
) RETURNS UUID AS $$
DECLARE
    v_acct     UUID;
    v_mask     TEXT;
    v_owner_of UUID;
    v_id       UUID;
BEGIN
    SELECT r.account_id, r.owner_name_masked INTO v_acct, v_mask
    FROM resolve_account_by_iban(p_iban) r;

    SELECT user_id INTO v_owner_of FROM accounts WHERE id = v_acct;
    IF v_owner_of = p_owner THEN
        RAISE EXCEPTION 'cannot add your own account as a beneficiary';
    END IF;

    INSERT INTO beneficiaries (owner_user_id, label, credit_account_id, iban, owner_name_masked)
    VALUES (p_owner, p_label, v_acct, p_iban, v_mask)
    RETURNING id INTO v_id;
    RETURN v_id;
END;
$$ LANGUAGE plpgsql;

-- delete_beneficiary: scoped delete; raises (-> 404) if it isn't the caller's.
CREATE OR REPLACE FUNCTION delete_beneficiary(p_owner UUID, p_id UUID)
RETURNS VOID AS $$
BEGIN
    DELETE FROM beneficiaries WHERE id = p_id AND owner_user_id = p_owner;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'beneficiary % not found', p_id;
    END IF;
END;
$$ LANGUAGE plpgsql;

-- ─────────────────────────────────────────────────────────────────────────────
-- Guided-transfer "mule menu" resolver
-- Returns a MENU of up to N candidate accounts belonging to OTHER users — a random,
-- shuffled pool drawn from the active guided_scenarios short-list (operator/seed-
-- controlled mule targets). The client picks one at random; an empty result means
-- "no stranger eligible — the client falls back to the caller's own account".
-- ─────────────────────────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION suggest_transfer_destinations(
    p_caller       UUID,
    p_from_account UUID    DEFAULT NULL,  -- excluded; resolver substitutes the caller's default
    p_amount_minor BIGINT  DEFAULT 0      -- a scenario only enters the pool once amount >= its min
)
RETURNS TABLE (
    account_id        UUID,
    iban              VARCHAR,
    owner_name_masked TEXT,
    reason            TEXT,
    scenario          TEXT,
    source            TEXT
) AS $$
DECLARE
    v_from UUID := p_from_account;
BEGIN
    -- Effective debit account to exclude: explicit, else the caller's default.
    IF v_from IS NULL THEN
        SELECT id INTO v_from FROM accounts
         WHERE user_id = p_caller AND kind = 'customer' AND is_default
         LIMIT 1;
    END IF;

    -- Eligible pool = the ACTIVE guided_scenarios short-list (the mule targets),
    -- NOT arbitrary peers — the mule is operator/seed-controlled by design. A target
    -- qualifies when its scenario matches the caller + amount (per-user targeting
    -- beats global), the target is an active customer account owned by ANOTHER user
    -- (every option is a third party), and it isn't the debit account. DISTINCT ON
    -- collapses multiple scenarios pointing at the same account to one row (keeping
    -- the per-user/priority/recency winner). Then sample up to 3 at random.
    RETURN QUERY
    WITH eligible AS (
        SELECT DISTINCT ON (a.id)
               a.id, a.iban, a.user_id, gs.reason AS reason, gs.name AS scenario
        FROM guided_scenarios gs
        JOIN accounts a ON a.id = gs.target_account_id
        WHERE gs.active
          AND COALESCE(p_amount_minor, 0) >= gs.min_amount_minor
          AND (gs.target_user_id IS NULL OR gs.target_user_id = p_caller)
          AND a.kind = 'customer' AND a.status = 'active'
          AND a.user_id <> p_caller
          AND (v_from IS NULL OR a.id <> v_from)
        ORDER BY a.id, (gs.target_user_id IS NOT NULL) DESC, gs.priority DESC, gs.created_at DESC
    )
    SELECT e.id, e.iban,
           mask_name((SELECT u.full_name FROM users u WHERE u.id = e.user_id)),
           e.reason, e.scenario, 'scenario'::text
    FROM eligible e
    ORDER BY random()
    LIMIT 3;
END;
$$ LANGUAGE plpgsql VOLATILE;

-- ─────────────────────────────────────────────────────────────────────────────
-- Dispute functions
-- ─────────────────────────────────────────────────────────────────────────────

-- raise_dispute: open a case. Caller must be a party to the transfer (debit or
-- credit owner). Records a fraud signal in admin_actions (flag-only, no auto-freeze).
-- The partial unique index enforces "no duplicate open dispute" (23505 -> 409).
--   not a party / unknown transfer -> P0001 "not found" -> 404 (existence hidden)
--   transfer not settled            -> check_violation 23514 -> 422
CREATE OR REPLACE FUNCTION raise_dispute(
    p_transfer_id UUID,
    p_raiser      UUID,
    p_category    dispute_category DEFAULT 'unrecognised',
    p_reason      TEXT DEFAULT ''
) RETURNS UUID AS $$
DECLARE
    v_t        transfers%ROWTYPE;
    v_id       UUID;
    v_is_party BOOLEAN;
BEGIN
    SELECT * INTO v_t FROM transfers WHERE id = p_transfer_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'transfer % not found', p_transfer_id;     -- -> 404
    END IF;

    -- Party check: raiser owns either side of the transfer.
    SELECT EXISTS (
        SELECT 1 FROM accounts a
        WHERE a.id IN (v_t.debit_account_id, v_t.credit_account_id)
          AND a.user_id = p_raiser
    ) INTO v_is_party;
    IF NOT v_is_party THEN
        RAISE EXCEPTION 'transfer % not found', p_transfer_id;     -- -> 404 (don't reveal existence)
    END IF;

    -- Only a settled (posted/reversed) transfer is disputable; a pending one is
    -- cancellable instead.
    IF v_t.status NOT IN ('posted', 'reversed') THEN
        RAISE EXCEPTION 'cannot dispute a transfer in state %', v_t.status
            USING ERRCODE = 'check_violation';                    -- -> 422
    END IF;

    INSERT INTO disputes (transfer_id, raised_by_user_id, category, reason)
    VALUES (p_transfer_id, p_raiser, p_category, COALESCE(p_reason, ''))
    RETURNING id INTO v_id;                                        -- dup open -> 23505 -> 409

    -- Server-side fraud hook: an auditable signal alongside the ledger (flag-only).
    INSERT INTO admin_actions (actor_user_id, action, target_id, detail)
    VALUES (p_raiser, 'dispute_raised', p_transfer_id,
            jsonb_build_object('dispute_id', v_id, 'category', p_category));

    RETURN v_id;
END;
$$ LANGUAGE plpgsql;

-- resolve_dispute: operator transition (state machine). Records the resolver +
-- note; appends an admin_actions audit row. Illegal transitions raise P0001 (-> 409);
-- unknown id -> P0001 "not found" -> 404.
--   open          -> under_review | resolved | rejected
--   under_review  -> resolved | rejected   (under_review->under_review is a no-op)
--   resolved/rejected -> (terminal) -> 409
CREATE OR REPLACE FUNCTION resolve_dispute(
    p_dispute_id UUID,
    p_resolver   UUID,
    p_status     dispute_status,
    p_note       TEXT DEFAULT ''
) RETURNS dispute_status AS $$
DECLARE v_cur dispute_status;
BEGIN
    SELECT status INTO v_cur FROM disputes WHERE id = p_dispute_id FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'dispute % not found', p_dispute_id;          -- -> 404
    END IF;

    IF p_status NOT IN ('under_review', 'resolved', 'rejected') THEN
        RAISE EXCEPTION 'cannot set dispute to %', p_status;          -- -> 409 (defensive; API enum-guards too)
    END IF;
    IF v_cur IN ('resolved', 'rejected') THEN
        RAISE EXCEPTION 'cannot transition a % dispute', v_cur;       -- -> 409
    END IF;
    IF v_cur = 'under_review' AND p_status = 'under_review' THEN
        RETURN v_cur;  -- no-op
    END IF;

    UPDATE disputes
       SET status           = p_status,
           resolver_user_id = p_resolver,
           resolution_note  = CASE WHEN p_status IN ('resolved','rejected')
                                   THEN COALESCE(NULLIF(p_note,''), resolution_note)
                                   ELSE resolution_note END
     WHERE id = p_dispute_id;

    INSERT INTO admin_actions (actor_user_id, action, target_id, detail)
    VALUES (p_resolver, 'dispute_' || p_status::text, p_dispute_id,
            jsonb_build_object('note', COALESCE(p_note,'')));

    RETURN p_status;
END;
$$ LANGUAGE plpgsql;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP FUNCTION IF EXISTS resolve_dispute(UUID, UUID, dispute_status, TEXT);
DROP FUNCTION IF EXISTS raise_dispute(UUID, UUID, dispute_category, TEXT);
DROP FUNCTION IF EXISTS suggest_transfer_destinations(UUID, UUID, BIGINT);
DROP FUNCTION IF EXISTS delete_beneficiary(UUID, UUID);
DROP FUNCTION IF EXISTS add_beneficiary(UUID, TEXT, VARCHAR);
DROP FUNCTION IF EXISTS resolve_account_by_iban(VARCHAR);
DROP TABLE IF EXISTS disputes;
DROP INDEX IF EXISTS idx_guided_scenarios_active;
DROP TABLE IF EXISTS guided_scenarios;
DROP TABLE IF EXISTS beneficiaries;
-- +goose StatementEnd
