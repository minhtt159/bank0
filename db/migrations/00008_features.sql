-- +goose Up
-- ─────────────────────────────────────────────────────────────────────────────
-- AUXILIARY FEATURES — payees, guided transfers, disputes & the events feed
-- Customer-facing extras layered on top of core banking, none of which hold money
-- state: saved beneficiaries (with confirmation-of-payee), the guided-transfer
-- "mule menu" demo (fraudbank's APP-scam simulation), the dispute case workflow,
-- and the per-user notification feed (events — an append-only projection written
-- in the same txn as its cause; emitting sites in 00003/00005 and here).
-- Their functions lean on the IBAN primitives (00002), the masking helper, and the
-- core tables/functions (accounts 00004, transfers 00005, admin_actions 00006). The
-- dispute/event taxonomy enums live in 00001; the shared set_updated_at() trigger
-- fn (00004) backs trg_disputes_updated_at.
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
    -- PSR/APP-scam claim machine (Rec 12): the regulatory clock + outcome.
    scam_type               scam_type,                                   -- NULL = not claimed as a scam
    sla_due_at              TIMESTAMPTZ,                                 -- business-day deadline (raise_dispute)
    decision                dispute_decision NOT NULL DEFAULT 'pending',
    reimbursed_amount_minor BIGINT CHECK (reimbursed_amount_minor >= 0), -- actual payout (net of excess)
    vulnerable_flag         BOOLEAN NOT NULL DEFAULT FALSE,              -- PSR: excess waived
    -- simulated interbank recall (pacs.004) — state only, the core is closed.
    recall_status           recall_status NOT NULL DEFAULT 'none',
    recall_reason           TEXT NOT NULL DEFAULT '',                    -- e.g. FRAD
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

-- resolve_account_by_iban: confirmation-of-payee with the SERVER-SIDE verdict
-- (CoP/VOP four-outcome model; spec-banking-grade-hardening Rec 9). The caller
-- may supply the name the customer typed (p_name_hint); the DB — not each
-- client — decides match / close_match / no_match / unable, so web/iOS/Android
-- gate identically by construction. On a close_match the ACTUAL registered name
-- is returned (suggested_name) so the customer can fix a typo — that disclosure
-- is what CoP/VOP mandate; on no_match nothing is revealed beyond the mask.
-- gate: ok = proceed; awaiting_acknowledgement = warn, customer may proceed
-- (the VOP liability pivot); 'blocked' is reserved for the future risk seam.
-- Matching: exact after whitespace/case normalization -> match; pg_trgm
-- similarity >= 0.55 -> close_match (threshold is a product knob, hedged — the
-- EPC wire tokens are not public). Raises (-> 404) if not found / inactive.
CREATE OR REPLACE FUNCTION resolve_account_by_iban(
    p_iban      VARCHAR,
    p_name_hint TEXT DEFAULT NULL,
    p_caller    UUID DEFAULT NULL
)
RETURNS TABLE (account_id UUID, iban VARCHAR, owner_name_masked TEXT,
               match_result TEXT, reason_code TEXT, suggested_name TEXT,
               account_type TEXT, gate TEXT, checked_at TIMESTAMPTZ,
               recipient_risk TEXT, mule_suspected BOOLEAN,
               signals TEXT[], is_first_payment_to_payee BOOLEAN) AS $$
DECLARE
    v_id      UUID;
    v_iban    VARCHAR;
    v_full    TEXT;
    v_created TIMESTAMPTZ;
    v_hint    TEXT;
    v_reg     TEXT;
    v_match   TEXT;
    v_reason  TEXT;
    v_suggest TEXT;
    v_gate    TEXT;
    -- recipient-risk signals (Rec 11)
    v_signals TEXT[] := '{}';
    v_mule    BOOLEAN := FALSE;
    v_first   BOOLEAN := FALSE;
    v_risk    TEXT;
BEGIN
    SELECT a.id, a.iban, u.full_name, a.created_at INTO v_id, v_iban, v_full, v_created
    FROM accounts a
    JOIN users u ON u.id = a.user_id
    WHERE a.iban = p_iban AND a.kind = 'customer' AND a.status = 'active';
    IF NOT FOUND THEN
        RAISE EXCEPTION 'account with iban % not found', p_iban;
    END IF;

    IF p_name_hint IS NULL OR btrim(p_name_hint) = '' THEN
        v_match := 'unable';  v_reason := 'NAME_NOT_SUPPLIED';  v_gate := 'awaiting_acknowledgement';
    ELSE
        v_hint := lower(regexp_replace(btrim(p_name_hint), '\s+', ' ', 'g'));
        v_reg  := lower(regexp_replace(btrim(v_full),      '\s+', ' ', 'g'));
        IF v_hint = v_reg THEN
            v_match := 'match';       v_reason := 'MATCH';       v_gate := 'ok';
        ELSIF similarity(v_hint, v_reg) >= 0.55 THEN
            v_match := 'close_match'; v_reason := 'CLOSE_MATCH'; v_gate := 'awaiting_acknowledgement';
            v_suggest := v_full;   -- the one deliberate disclosure (CoP close-match)
        ELSE
            v_match := 'no_match';    v_reason := 'NO_MATCH';    v_gate := 'awaiting_acknowledgement';
        END IF;
    END IF;

    -- Recipient-risk signals (Rec 11). The operator-flagged mule pool
    -- (guided_scenarios) IS the rule seam; a destination sitting in it — or on
    -- the credit side of a live fraud dispute — resolves high. The caller-scoped
    -- signals (new payee / first payment) power first-payment friction.
    IF EXISTS (SELECT 1 FROM guided_scenarios gs
                WHERE gs.target_account_id = v_id AND gs.active) THEN
        v_signals := v_signals || 'mule_flagged'::text;  v_mule := TRUE;
    END IF;
    IF EXISTS (SELECT 1 FROM disputes d
                JOIN transfers t ON t.id = d.transfer_id
                WHERE t.credit_account_id = v_id
                  AND d.status IN ('open', 'under_review')
                  AND d.category IN ('fraud', 'unrecognised')) THEN
        v_signals := v_signals || 'reported'::text;  v_mule := TRUE;
    END IF;
    IF v_created > now() - INTERVAL '30 days' THEN
        v_signals := v_signals || 'recently_opened'::text;
    END IF;
    IF p_caller IS NOT NULL THEN
        IF NOT EXISTS (SELECT 1 FROM beneficiaries b
                        WHERE b.owner_user_id = p_caller AND b.credit_account_id = v_id) THEN
            v_signals := v_signals || 'new_payee'::text;
        END IF;
        v_first := NOT EXISTS (
            SELECT 1 FROM transfers t
            JOIN accounts da ON da.id = t.debit_account_id
            WHERE da.user_id = p_caller AND t.credit_account_id = v_id
              AND t.status IN ('posted', 'reversed'));
        IF v_first THEN
            v_signals := v_signals || 'first_payment'::text;
        END IF;
    END IF;
    v_risk := CASE
        WHEN v_mule THEN 'high'
        WHEN v_signals @> ARRAY['new_payee','first_payment','recently_opened'] THEN 'medium'
        ELSE 'low' END;

    RETURN QUERY SELECT v_id, v_iban, mask_name(v_full), v_match, v_reason,
                        v_suggest, 'personal'::text, v_gate, now(),
                        v_risk, v_mule, v_signals, v_first;
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
    p_reason      TEXT DEFAULT '',
    p_scam_type   scam_type DEFAULT NULL
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

    -- sla_due_at: the PSR-style business-day clock (15 BBD, mirroring the SEPA
    -- beneficiary-bank answer deadline). Weekends skipped; no holiday calendar.
    INSERT INTO disputes (transfer_id, raised_by_user_id, category, reason, scam_type, sla_due_at)
    VALUES (p_transfer_id, p_raiser, p_category, COALESCE(p_reason, ''), p_scam_type,
            add_business_days(now(), 15))
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
DECLARE v_d disputes%ROWTYPE;
BEGIN
    SELECT * INTO v_d FROM disputes WHERE id = p_dispute_id FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'dispute % not found', p_dispute_id;          -- -> 404
    END IF;

    IF p_status NOT IN ('under_review', 'resolved', 'rejected') THEN
        RAISE EXCEPTION 'cannot set dispute to %', p_status;          -- -> 409 (defensive; API enum-guards too)
    END IF;
    IF v_d.status IN ('resolved', 'rejected') THEN
        RAISE EXCEPTION 'cannot transition a % dispute', v_d.status;  -- -> 409
    END IF;
    IF v_d.status = 'under_review' AND p_status = 'under_review' THEN
        RETURN v_d.status;  -- no-op
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

    -- Notify the filer, in the same txn as the transition. Direct INSERT (not
    -- emit_event): dispute events are one per STATUS CHANGE, so they are exempt
    -- from the money events' one-per-transfer uniqueness (partial index below).
    INSERT INTO events (user_id, type, title, body, related_transfer_id, data)
    VALUES (v_d.raised_by_user_id, 'dispute.updated', 'Dispute updated',
            'Your dispute is now ' || p_status::text || '.',
            v_d.transfer_id,
            jsonb_build_object('dispute_id', p_dispute_id, 'status', p_status));

    RETURN p_status;
END;
$$ LANGUAGE plpgsql;

-- assess_transfer_risk: the server-side TRA seam (Rec 15). Scores a transfer
-- ATTEMPT from what the bank already knows — velocity, first-payment, mule/
-- reported destination, account age — and emits a band the step-up gate ORs
-- into its trigger set. The client SDK is advisory input only; THIS decision is
-- authoritative. Deliberately transparent scoring, not ML:
--   +3 destination is an operator-flagged mule or on a live fraud dispute
--   +2 velocity count: >= 10 debits from the caller in the trailing 24h
--   +2 velocity value: 24h debits + this amount >= 5x the 90d daily average
--      (with a €100/day floor so a quiet account isn't gated by its first coffee)
--   +1 first payment from this caller to this destination
--   +1 debit account younger than 7 days
-- band: >= 4 high, >= 2 medium, else low.
-- ponytail: fixed weights; lift them into a rule table when tuning matters.
CREATE OR REPLACE FUNCTION assess_transfer_risk(
    p_caller       UUID,
    p_debit        UUID,
    p_credit       UUID,
    p_amount_minor BIGINT
) RETURNS TABLE (risk_band TEXT, score INT, reasons TEXT[]) AS $$
DECLARE
    v_score   INT := 0;
    v_reasons TEXT[] := '{}';
    v_count24 INT;
    v_sum24   BIGINT;
    v_avg90   NUMERIC;
BEGIN
    IF EXISTS (SELECT 1 FROM guided_scenarios gs
                WHERE gs.target_account_id = p_credit AND gs.active)
       OR EXISTS (SELECT 1 FROM disputes d
                   JOIN transfers t ON t.id = d.transfer_id
                   WHERE t.credit_account_id = p_credit
                     AND d.status IN ('open', 'under_review')
                     AND d.category IN ('fraud', 'unrecognised')) THEN
        v_score := v_score + 3;  v_reasons := v_reasons || 'destination_flagged'::text;
    END IF;

    SELECT count(*), COALESCE(sum(t.amount_minor), 0) INTO v_count24, v_sum24
      FROM transfers t
      JOIN accounts da ON da.id = t.debit_account_id
     WHERE da.user_id = p_caller
       AND t.requested_at > now() - INTERVAL '24 hours'
       AND t.status <> 'canceled';
    IF v_count24 >= 10 THEN
        v_score := v_score + 2;  v_reasons := v_reasons || 'velocity_count_24h'::text;
    END IF;

    SELECT COALESCE(sum(t.amount_minor), 0) / 90.0 INTO v_avg90
      FROM transfers t
      JOIN accounts da ON da.id = t.debit_account_id
     WHERE da.user_id = p_caller
       AND t.status IN ('posted', 'reversed')
       AND t.requested_at > now() - INTERVAL '90 days';
    IF (v_sum24 + p_amount_minor) >= 5 * GREATEST(v_avg90, 10000) THEN
        v_score := v_score + 2;  v_reasons := v_reasons || 'velocity_value_24h'::text;
    END IF;

    IF NOT EXISTS (SELECT 1 FROM transfers t
                    JOIN accounts da ON da.id = t.debit_account_id
                   WHERE da.user_id = p_caller AND t.credit_account_id = p_credit
                     AND t.status IN ('posted', 'reversed')) THEN
        v_score := v_score + 1;  v_reasons := v_reasons || 'first_payment_to_payee'::text;
    END IF;

    IF EXISTS (SELECT 1 FROM accounts a
                WHERE a.id = p_debit AND a.created_at > now() - INTERVAL '7 days') THEN
        v_score := v_score + 1;  v_reasons := v_reasons || 'new_debit_account'::text;
    END IF;

    RETURN QUERY SELECT CASE WHEN v_score >= 4 THEN 'high'
                             WHEN v_score >= 2 THEN 'medium'
                             ELSE 'low' END,
                        v_score, v_reasons;
END;
$$ LANGUAGE plpgsql STABLE;

-- decide_dispute: the PSR claim OUTCOME (Rec 12) — reimburse (full/partial) or
-- decline, in one transaction. A reimbursement is REAL MONEY: a transfer from
-- EXTERNAL_CLEARING to the disputed transfer's debit (victim) account, kind
-- 'adjustment', idempotency key derived from the dispute id — so the books stay
-- zero-sum, reconcile() still holds, and a replayed decide can never pay twice
-- (the state machine blocks it first; the key is the belt).
-- Policy (bank_settings): payout = min(requested, cap) minus the excess, except
-- for vulnerable customers (excess waived, PSR-style). Payout may not exceed the
-- disputed amount. Only an open/under_review dispute can be decided.
--   * unknown id                 -> P0001 'not found'   -> 404
--   * already terminal/decided   -> P0001 'cannot ...'  -> 409
--   * bad amounts                -> check_violation     -> 422
CREATE OR REPLACE FUNCTION decide_dispute(
    p_dispute_id      UUID,
    p_resolver        UUID,
    p_decision        dispute_decision,
    p_reimburse_minor BIGINT  DEFAULT NULL,
    p_vulnerable      BOOLEAN DEFAULT NULL,
    p_note            TEXT    DEFAULT ''
) RETURNS BIGINT AS $$
DECLARE
    v_d       disputes%ROWTYPE;
    v_t       transfers%ROWTYPE;
    v_cap     BIGINT;
    v_excess  BIGINT;
    v_vuln    BOOLEAN;
    v_payout  BIGINT := 0;
    v_ext     UUID;
    v_status  dispute_status;
BEGIN
    SELECT * INTO v_d FROM disputes WHERE id = p_dispute_id FOR UPDATE;
    IF NOT FOUND THEN RAISE EXCEPTION 'dispute % not found', p_dispute_id; END IF;
    IF v_d.status NOT IN ('open', 'under_review') THEN
        RAISE EXCEPTION 'cannot decide a % dispute', v_d.status;             -- -> 409
    END IF;
    IF p_decision = 'pending' THEN
        RAISE EXCEPTION 'decision must be reimbursed, partially_reimbursed or declined'
            USING ERRCODE = 'check_violation';
    END IF;

    SELECT * INTO v_t FROM transfers WHERE id = v_d.transfer_id;
    v_vuln := COALESCE(p_vulnerable, v_d.vulnerable_flag);

    IF p_decision IN ('reimbursed', 'partially_reimbursed') THEN
        IF p_reimburse_minor IS NULL OR p_reimburse_minor <= 0 THEN
            RAISE EXCEPTION 'reimbursed_amount_minor must be > 0'
                USING ERRCODE = 'check_violation';
        END IF;
        IF p_reimburse_minor > v_t.amount_minor THEN
            RAISE EXCEPTION 'reimbursement exceeds the disputed amount'
                USING ERRCODE = 'check_violation';
        END IF;
        SELECT reimbursement_cap_minor, reimbursement_excess_minor
          INTO v_cap, v_excess FROM bank_settings WHERE id;
        v_payout := LEAST(p_reimburse_minor, v_cap);
        IF NOT v_vuln THEN
            v_payout := GREATEST(v_payout - v_excess, 0);
        END IF;
        IF v_payout > 0 THEN
            SELECT id INTO v_ext FROM accounts WHERE system_code = 'EXTERNAL_CLEARING';
            IF NOT FOUND THEN RAISE EXCEPTION 'EXTERNAL_CLEARING system account missing (run seed)'; END IF;
            PERFORM transfer('dispute-reimburse-' || p_dispute_id::text,
                             v_ext, v_t.debit_account_id, v_payout,
                             'dispute reimbursement ' || p_dispute_id::text, 'adjustment');
        END IF;
        v_status := 'resolved';
    ELSE
        v_status := 'rejected';
    END IF;

    UPDATE disputes
       SET status = v_status,
           decision = p_decision,
           reimbursed_amount_minor = CASE WHEN p_decision = 'declined' THEN NULL ELSE v_payout END,
           vulnerable_flag  = v_vuln,
           resolver_user_id = p_resolver,
           resolution_note  = COALESCE(NULLIF(p_note, ''), resolution_note)
     WHERE id = p_dispute_id;

    INSERT INTO admin_actions (actor_user_id, action, target_id, detail)
    VALUES (p_resolver, 'dispute_decided', p_dispute_id,
            jsonb_build_object('decision', p_decision, 'payout_minor', v_payout,
                               'vulnerable', v_vuln, 'note', COALESCE(p_note, '')));

    INSERT INTO events (user_id, type, title, body, related_transfer_id, data)
    VALUES (v_d.raised_by_user_id, 'dispute.updated', 'Dispute decided',
            CASE WHEN p_decision = 'declined' THEN 'Your claim was declined.'
                 ELSE 'Your claim was accepted.' END,
            v_d.transfer_id,
            jsonb_build_object('dispute_id', p_dispute_id, 'status', v_status,
                               'decision', p_decision, 'payout_minor', v_payout));

    RETURN v_payout;
END;
$$ LANGUAGE plpgsql;

-- set_dispute_recall: the SIMULATED interbank recall (pacs.004) state machine.
-- none -> requested -> funds_returned | refused. May trail the decision (a
-- recall answer can arrive after reimbursement). Audited + notified.
CREATE OR REPLACE FUNCTION set_dispute_recall(
    p_dispute_id UUID,
    p_actor      UUID,
    p_status     recall_status,
    p_reason     TEXT DEFAULT ''
) RETURNS recall_status AS $$
DECLARE v_d disputes%ROWTYPE;
BEGIN
    SELECT * INTO v_d FROM disputes WHERE id = p_dispute_id FOR UPDATE;
    IF NOT FOUND THEN RAISE EXCEPTION 'dispute % not found', p_dispute_id; END IF;
    IF p_status = 'none' THEN
        RAISE EXCEPTION 'cannot transition a recall back to none';           -- -> 409
    END IF;
    IF p_status = 'requested' AND v_d.recall_status <> 'none' THEN
        RAISE EXCEPTION 'cannot re-request a % recall', v_d.recall_status;   -- -> 409
    END IF;
    IF p_status IN ('funds_returned', 'refused') AND v_d.recall_status <> 'requested' THEN
        RAISE EXCEPTION 'cannot answer a recall that is %', v_d.recall_status; -- -> 409
    END IF;

    UPDATE disputes SET recall_status = p_status,
                        recall_reason = COALESCE(NULLIF(p_reason, ''), recall_reason)
     WHERE id = p_dispute_id;

    INSERT INTO admin_actions (actor_user_id, action, target_id, detail)
    VALUES (p_actor, 'dispute_recall_' || p_status::text, p_dispute_id,
            jsonb_build_object('reason', COALESCE(p_reason, '')));

    INSERT INTO events (user_id, type, title, body, related_transfer_id, data)
    VALUES (v_d.raised_by_user_id, 'dispute.updated', 'Recall update',
            'The recall on your disputed payment is now ' || replace(p_status::text, '_', ' ') || '.',
            v_d.transfer_id,
            jsonb_build_object('dispute_id', p_dispute_id, 'recall_status', p_status));

    RETURN p_status;
END;
$$ LANGUAGE plpgsql;

-- +goose StatementEnd

-- ─────────────────────────────────────────────────────────────────────────────
-- events  (per-user notification feed — an append-only PROJECTION, not a second
-- source of truth for money; a lost event never corrupts the ledger)
-- Rows are written INSIDE the transaction that owns the source transition:
-- post_transfer (00005) emits transfer.posted / payment.incoming,
-- issue_refresh_token (00003) emits device.new, resolve_dispute (above) emits
-- dispute.updated. The event and its cause commit or roll back together.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE events (
    id                  UUID PRIMARY KEY DEFAULT uuidv7(),      -- UUIDv7: time-ordered keyset tiebreak
    user_id             UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    type                event_type NOT NULL,
    title               TEXT NOT NULL DEFAULT '',
    body                TEXT NOT NULL DEFAULT '',
    related_transfer_id UUID REFERENCES transfers(id) ON DELETE SET NULL,
    related_account_id  UUID REFERENCES accounts(id)  ON DELETE SET NULL,
    data                JSONB NOT NULL DEFAULT '{}',
    read_at             TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Feed read path: keyset (created_at, id) DESC per user.
CREATE INDEX idx_events_user_created ON events (user_id, created_at DESC, id DESC);
-- Unread badge / unread_only filter (partial: unread rows only).
CREATE INDEX idx_events_user_unread  ON events (user_id) WHERE read_at IS NULL;
-- Idempotent MONEY emission: at most one posted/incoming event per (user,
-- transfer). Partial so dispute.updated (one per status change) and device.new
-- (NULL transfer) are exempt.
CREATE UNIQUE INDEX uq_events_money_once ON events (user_id, type, related_transfer_id)
    WHERE type IN ('transfer.posted', 'payment.incoming');
-- Idempotent DEVICE emission: one device.new per refresh-token family.
CREATE UNIQUE INDEX uq_events_device_family ON events ((data->>'family_id'))
    WHERE type = 'device.new';

-- +goose StatementBegin

-- events_block_mutation: the feed is a record of things that happened. Only
-- read_at may ever change; deletes are blocked (user-cascade is the sole removal).
-- Mirrors ledger_block_mutation (00005).
CREATE OR REPLACE FUNCTION events_block_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'events is append-only (DELETE blocked)' USING ERRCODE = 'restrict_violation';
    END IF;
    IF NEW.id IS DISTINCT FROM OLD.id
       OR NEW.user_id IS DISTINCT FROM OLD.user_id
       OR NEW.type IS DISTINCT FROM OLD.type
       OR NEW.title IS DISTINCT FROM OLD.title
       OR NEW.body IS DISTINCT FROM OLD.body
       OR NEW.related_transfer_id IS DISTINCT FROM OLD.related_transfer_id
       OR NEW.related_account_id IS DISTINCT FROM OLD.related_account_id
       OR NEW.data IS DISTINCT FROM OLD.data
       OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'events rows are immutable except read_at' USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- emit_event: idempotent insert for the MONEY event types (the partial unique
-- absorbs a re-emit; replays of the source move never reach post_transfer
-- anyway). Returns the event id (new or existing). Same-txn safe.
CREATE OR REPLACE FUNCTION emit_event(
    p_user_id      UUID,
    p_type         event_type,
    p_title        TEXT,
    p_body         TEXT,
    p_transfer_id  UUID  DEFAULT NULL,
    p_account_id   UUID  DEFAULT NULL,
    p_data         JSONB DEFAULT '{}'
) RETURNS UUID AS $$
DECLARE v_id UUID;
BEGIN
    INSERT INTO events (user_id, type, title, body, related_transfer_id, related_account_id, data)
    VALUES (p_user_id, p_type, COALESCE(p_title,''), COALESCE(p_body,''), p_transfer_id, p_account_id, COALESCE(p_data,'{}'::jsonb))
    ON CONFLICT (user_id, type, related_transfer_id)
        WHERE type IN ('transfer.posted', 'payment.incoming')
        DO NOTHING
    RETURNING id INTO v_id;
    IF v_id IS NULL THEN
        SELECT id INTO v_id FROM events
         WHERE user_id = p_user_id AND type = p_type
           AND related_transfer_id IS NOT DISTINCT FROM p_transfer_id
         ORDER BY created_at DESC LIMIT 1;
    END IF;
    RETURN v_id;
END;
$$ LANGUAGE plpgsql;

-- mark_events_read: set read_at on the caller's unread events at/older than a
-- cursor position, or all when p_cursor_ts is NULL. Returns the count touched.
CREATE OR REPLACE FUNCTION mark_events_read(
    p_user_id   UUID,
    p_cursor_ts TIMESTAMPTZ DEFAULT NULL,
    p_cursor_id UUID        DEFAULT NULL
) RETURNS INT AS $$
DECLARE v_n INT;
BEGIN
    UPDATE events SET read_at = now()
     WHERE user_id = p_user_id AND read_at IS NULL
       AND (p_cursor_ts IS NULL
            OR (created_at, id) <= (p_cursor_ts, COALESCE(p_cursor_id, 'ffffffff-ffff-ffff-ffff-ffffffffffff')));
    GET DIAGNOSTICS v_n = ROW_COUNT;
    RETURN v_n;
END;
$$ LANGUAGE plpgsql;

-- +goose StatementEnd

CREATE TRIGGER trg_events_immutable
    BEFORE UPDATE OR DELETE ON events
    FOR EACH ROW EXECUTE FUNCTION events_block_mutation();

-- ─────────────────────────────────────────────────────────────────────────────
-- warning_acks  (fraud-warning evidence — the IPR/VOP liability pivot)
-- "The customer was shown warning X about payment Y and chose to proceed."
-- One row per shown/acknowledged warning, captured BEFORE the money moves and
-- tied to the attempt by (user, debit account, counterparty, amount). Append-only
-- (same discipline as events/ledger): liability evidence must not be rewritable.
-- This is the input to any future reimbursement / consumer-standard-of-caution
-- file (spec-banking-grade-hardening Rec 10; Rec 26 joins it to the audit feed).
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE warning_acks (
    id                UUID PRIMARY KEY DEFAULT uuidv7(),
    user_id           UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    category          TEXT NOT NULL,                 -- cop_no_match | cop_close_match | cop_unable | guided_steer | high_value | other
    reason_code       TEXT NOT NULL DEFAULT '',      -- machine token echoed from the warning (e.g. NO_MATCH)
    acknowledged      BOOLEAN NOT NULL DEFAULT TRUE, -- FALSE = warning shown, customer backed out
    debit_account_id  UUID REFERENCES accounts(id) ON DELETE SET NULL,
    counterparty_iban TEXT NOT NULL DEFAULT '',
    amount_minor      BIGINT,
    device            TEXT NOT NULL DEFAULT '',      -- client-supplied device/platform label
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (category IN ('cop_no_match','cop_close_match','cop_unable','guided_steer','high_value','other')),
    CHECK (amount_minor IS NULL OR amount_minor > 0)
);
CREATE INDEX idx_warning_acks_user ON warning_acks (user_id, created_at DESC);

-- +goose StatementBegin

-- record_warning_ack: persist one piece of warning evidence for the caller. The
-- debit account, when given, must belong to the caller (42501 -> 403) so evidence
-- can't be planted on someone else's account.
CREATE OR REPLACE FUNCTION record_warning_ack(
    p_user_id           UUID,
    p_category          TEXT,
    p_reason_code       TEXT    DEFAULT '',
    p_acknowledged      BOOLEAN DEFAULT TRUE,
    p_debit_account_id  UUID    DEFAULT NULL,
    p_counterparty_iban TEXT    DEFAULT '',
    p_amount_minor      BIGINT  DEFAULT NULL,
    p_device            TEXT    DEFAULT ''
) RETURNS UUID AS $$
DECLARE v_owner UUID; v_id UUID;
BEGIN
    IF p_debit_account_id IS NOT NULL THEN
        SELECT user_id INTO v_owner FROM accounts WHERE id = p_debit_account_id;
        IF v_owner IS NULL OR v_owner <> p_user_id THEN
            RAISE EXCEPTION 'account does not belong to the caller' USING ERRCODE = '42501';
        END IF;
    END IF;
    INSERT INTO warning_acks (user_id, category, reason_code, acknowledged,
                              debit_account_id, counterparty_iban, amount_minor, device)
    VALUES (p_user_id, p_category, COALESCE(p_reason_code,''), COALESCE(p_acknowledged, TRUE),
            p_debit_account_id, COALESCE(p_counterparty_iban,''), p_amount_minor, COALESCE(p_device,''))
    RETURNING id INTO v_id;
    RETURN v_id;
END;
$$ LANGUAGE plpgsql;

-- warning_acks_block_mutation: evidence is written once. No UPDATE, no DELETE
-- (user-cascade is the sole removal path).
CREATE OR REPLACE FUNCTION warning_acks_block_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'warning_acks is append-only (% blocked)', TG_OP
        USING ERRCODE = 'restrict_violation';
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER trg_warning_acks_immutable
    BEFORE UPDATE OR DELETE ON warning_acks
    FOR EACH ROW EXECUTE FUNCTION warning_acks_block_mutation();

-- +goose Down
DROP TRIGGER IF EXISTS trg_warning_acks_immutable ON warning_acks;
-- +goose StatementBegin
DROP FUNCTION IF EXISTS warning_acks_block_mutation();
DROP FUNCTION IF EXISTS record_warning_ack(UUID, TEXT, TEXT, BOOLEAN, UUID, TEXT, BIGINT, TEXT);
-- +goose StatementEnd
DROP TABLE IF EXISTS warning_acks;
DROP TRIGGER IF EXISTS trg_events_immutable ON events;
-- +goose StatementBegin
DROP FUNCTION IF EXISTS mark_events_read(UUID, TIMESTAMPTZ, UUID);
DROP FUNCTION IF EXISTS emit_event(UUID, event_type, TEXT, TEXT, UUID, UUID, JSONB);
DROP FUNCTION IF EXISTS events_block_mutation();
DROP FUNCTION IF EXISTS set_dispute_recall(UUID, UUID, recall_status, TEXT);
DROP FUNCTION IF EXISTS decide_dispute(UUID, UUID, dispute_decision, BIGINT, BOOLEAN, TEXT);
DROP FUNCTION IF EXISTS assess_transfer_risk(UUID, UUID, UUID, BIGINT);
DROP FUNCTION IF EXISTS resolve_dispute(UUID, UUID, dispute_status, TEXT);
DROP FUNCTION IF EXISTS raise_dispute(UUID, UUID, dispute_category, TEXT, scam_type);
DROP FUNCTION IF EXISTS suggest_transfer_destinations(UUID, UUID, BIGINT);
DROP FUNCTION IF EXISTS delete_beneficiary(UUID, UUID);
DROP FUNCTION IF EXISTS add_beneficiary(UUID, TEXT, VARCHAR);
DROP FUNCTION IF EXISTS resolve_account_by_iban(VARCHAR, TEXT, UUID);
DROP TABLE IF EXISTS events;
DROP TABLE IF EXISTS disputes;
DROP INDEX IF EXISTS idx_guided_scenarios_active;
DROP TABLE IF EXISTS guided_scenarios;
DROP TABLE IF EXISTS beneficiaries;
-- +goose StatementEnd
