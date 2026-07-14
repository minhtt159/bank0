-- +goose Up
-- ─────────────────────────────────────────────────────────────────────────────
-- FRAUD / AML SEAM — risk scoring, warning evidence, rules, watchlist & the gate
-- The server-authoritative fraud stack the transfer() auto-post path (00008) ORs
-- into its decision: assess_transfer_risk (the transparent TRA score), warning_acks
-- (the IPR/VOP liability-evidence log), warning_rules (the operator-tweakable policy
-- table), watchlist_entries (AML name screening), screen_payment / assert_warning_ack,
-- and evaluate_transfer (the Rec 22 preflight/decision). IN-FILE ORDER IS LOAD-BEARING:
-- warning_rules must precede evaluate_transfer (which declares warning_rules%ROWTYPE)
-- and watchlist_entries must precede screen_payment (a LANGUAGE sql body validated at
-- CREATE). Cross-file refs (guided_scenarios/disputes/transfers/is_known_payee/
-- assert_caller_owns) are late-bound plpgsql or already created earlier.
-- ─────────────────────────────────────────────────────────────────────────────

-- +goose StatementBegin

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
-- p_exclude_transfer lets a submit-time caller drop the just-inserted pending
-- transfer from its OWN velocity math, so the intent preflight and the submit gate
-- compute the same band at a boundary (evaluate_transfer/transfer() pass it).
CREATE OR REPLACE FUNCTION assess_transfer_risk(
    p_caller           UUID,
    p_debit            UUID,
    p_credit           UUID,
    p_amount_minor     BIGINT,
    p_exclude_transfer UUID DEFAULT NULL
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
       AND t.status <> 'canceled'
       AND t.id IS DISTINCT FROM p_exclude_transfer;
    IF v_count24 >= 10 THEN
        v_score := v_score + 2;  v_reasons := v_reasons || 'velocity_count_24h'::text;
    END IF;

    SELECT COALESCE(sum(t.amount_minor), 0) / 90.0 INTO v_avg90
      FROM transfers t
      JOIN accounts da ON da.id = t.debit_account_id
     WHERE da.user_id = p_caller
       AND t.status IN ('posted', 'reversed')
       AND t.requested_at > now() - INTERVAL '90 days'
       AND t.id IS DISTINCT FROM p_exclude_transfer;
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

-- +goose StatementEnd

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
    category          TEXT NOT NULL,                 -- cop_no_match | cop_close_match | cop_unable | guided_steer | high_value | risk_warning | other
    reason_code       TEXT NOT NULL DEFAULT '',      -- machine token echoed from the warning (e.g. NO_MATCH)
    acknowledged      BOOLEAN NOT NULL DEFAULT TRUE, -- FALSE = warning shown, customer backed out
    debit_account_id  UUID REFERENCES accounts(id) ON DELETE SET NULL,
    counterparty_iban TEXT NOT NULL DEFAULT '',
    amount_minor      BIGINT,
    device            TEXT NOT NULL DEFAULT '',      -- client-supplied device/platform label
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (category IN ('cop_no_match','cop_close_match','cop_unable','guided_steer','high_value','risk_warning','other')),
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

-- ─────────────────────────────────────────────────────────────────────────────
-- warning_rules  (Rec 22 — the fraud-warning / decision policy table)
-- Lifts assess_transfer_risk's fixed weights into an OPERATOR-tweakable rule set:
-- a rule MATCHES a transfer when its non-null match keys hold (a specific reason
-- code fired, and/or the assessed band is at least match_min_band), and carries the
-- copy shown to the customer + the behaviour to apply (warn | review | block, plus
-- whether an acknowledgement is required and how long the cooling-off is). Ships
-- EMPTY — no rule matches, so evaluate_transfer degrades to today's allow/step_up
-- behaviour until an operator (or db/seed.sql) adds rules. Demo rules seed via
-- db/seed.sql, not here, so existing migration-only tests stay unaffected.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE warning_rules (
    id                  UUID PRIMARY KEY DEFAULT uuidv7(),
    match_reason_code   TEXT,                                   -- e.g. 'destination_flagged' (NULL = any)
    match_min_band      TEXT,                                   -- 'low'|'medium'|'high' (NULL = any)
    category            TEXT NOT NULL,                          -- mirrors warning_acks.category
    headline            TEXT NOT NULL DEFAULT '',
    body                TEXT NOT NULL DEFAULT '',
    severity            TEXT NOT NULL DEFAULT 'warning',        -- info | warning | critical
    decision            TEXT NOT NULL DEFAULT 'warn',           -- warn | review | block
    required_ack        BOOLEAN NOT NULL DEFAULT FALSE,
    cooling_off_seconds INT NOT NULL DEFAULT 0,
    priority            INT NOT NULL DEFAULT 0,                 -- higher wins among equal-severity matches
    active              BOOLEAN NOT NULL DEFAULT TRUE,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- at least one match key must be set, else a rule matches everything.
    CHECK (match_reason_code IS NOT NULL OR match_min_band IS NOT NULL),
    CHECK (match_min_band IS NULL OR match_min_band IN ('low','medium','high')),
    CHECK (category IN ('cop_no_match','cop_close_match','cop_unable','guided_steer','high_value','risk_warning','other')),
    CHECK (severity IN ('info','warning','critical')),
    CHECK (decision IN ('warn','review','block')),
    CHECK (cooling_off_seconds BETWEEN 0 AND 86400)
);
CREATE INDEX idx_warning_rules_active ON warning_rules (priority DESC) WHERE active;

CREATE TRIGGER trg_warning_rules_updated_at
    BEFORE UPDATE ON warning_rules FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ─────────────────────────────────────────────────────────────────────────────
-- watchlist_entries  (Rec 25 — the sanctions/AML name screening list)
-- A pattern matched (ILIKE) against a party's registered full_name. Ships EMPTY —
-- screen_payment then never matches, so transfer() behaves exactly as before until
-- an operator (or db/seed.sql) adds an entry.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE watchlist_entries (
    id          UUID PRIMARY KEY DEFAULT uuidv7(),
    pattern     TEXT NOT NULL,                                  -- ILIKE pattern against users.full_name
    reason      TEXT NOT NULL DEFAULT '',
    active      BOOLEAN NOT NULL DEFAULT TRUE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (pattern <> '')
);
CREATE INDEX idx_watchlist_active ON watchlist_entries (created_at DESC) WHERE active;

-- +goose StatementBegin

-- ─────────────────────────────────────────────────────────────────────────────
-- screen_payment: the AML name-screening seam (Rec 25). Returns the first active
-- watchlist hit against EITHER party's registered full_name (creditor preferred),
-- or NO ROWS when nothing matches (== today's behaviour, since the list ships empty).
-- ─────────────────────────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION screen_payment(p_debit UUID, p_credit UUID)
RETURNS TABLE (entry_id UUID, matched_name TEXT, side TEXT) AS $$
    SELECT w.id, u.full_name,
           CASE WHEN a.id = p_debit THEN 'debit' ELSE 'credit' END
    FROM watchlist_entries w
    JOIN accounts a ON a.id IN (p_debit, p_credit)
    JOIN users u ON u.id = a.user_id
    WHERE w.active AND u.full_name ILIKE w.pattern
    ORDER BY (a.id = p_credit) DESC
    LIMIT 1;
$$ LANGUAGE sql STABLE;

-- ─────────────────────────────────────────────────────────────────────────────
-- assert_warning_ack: enforce that the caller already acknowledged the required
-- warning for THIS exact payment (Rec 22). A qualifying warning_acks row matches on
-- (user, category, debit account, credit counterparty IBAN, exact amount) with
-- acknowledged = TRUE, and must be AGED past the cooling-off yet still fresh (within
-- cooling_off + 30 minutes) — so a customer can't pre-click far in advance nor reuse
-- a stale ack from a prior session. Missing/too-fresh/too-old/mismatched -> 23514.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION assert_warning_ack(
    p_user                UUID,
    p_category            TEXT,
    p_debit               UUID,
    p_credit              UUID,
    p_amount_minor        BIGINT,
    p_cooling_off_seconds INT
) RETURNS VOID AS $$
DECLARE
    v_iban TEXT;
    v_cool INTERVAL := make_interval(secs => COALESCE(p_cooling_off_seconds, 0));
BEGIN
    SELECT COALESCE(iban, system_code, '') INTO v_iban FROM accounts WHERE id = p_credit;
    IF NOT EXISTS (
        SELECT 1 FROM warning_acks wa
        WHERE wa.user_id = p_user
          AND wa.category = p_category
          AND wa.debit_account_id = p_debit
          AND wa.counterparty_iban = v_iban
          AND wa.amount_minor = p_amount_minor
          AND wa.acknowledged
          AND wa.created_at <= now() - v_cool
          AND wa.created_at >  now() - (v_cool + INTERVAL '30 minutes')
    ) THEN
        RAISE EXCEPTION 'warning acknowledgement required for this payment'
            USING ERRCODE = 'check_violation';
    END IF;
END;
$$ LANGUAGE plpgsql STABLE;

-- ─────────────────────────────────────────────────────────────────────────────
-- evaluate_transfer: the Rec 22 preflight/decision. Wraps assess_transfer_risk
-- (server-authoritative band + reason codes), picks the ONE best-matching active
-- warning_rule (block > review > warn by decision severity, then priority DESC, then
-- oldest), computes the step-up axis (a configured per-payment limit, OR a high band,
-- OR an unsaved payee -> 'otp'; the payee check is the SAME saved-beneficiary
-- predicate as the Go gate's IsKnownPayee so the preview never diverges from what
-- POST /transfers enforces), and collapses everything to a single
-- decision by precedence block > review > step_up > warn > allow. STABLE and
-- read-only; the numeric risk score is NEVER surfaced. Asserts the caller owns the
-- debit account (42501 -> 403) so it is safe to expose on the client intent endpoint.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION evaluate_transfer(
    p_caller              UUID,
    p_debit               UUID,
    p_credit              UUID,
    p_amount_minor        BIGINT,
    p_step_up_limit_minor BIGINT DEFAULT 0,
    p_exclude_transfer    UUID   DEFAULT NULL
) RETURNS TABLE (
    decision            TEXT,
    risk_band           TEXT,
    reason_codes        TEXT[],
    rule_id             UUID,
    category            TEXT,
    headline            TEXT,
    body                TEXT,
    severity            TEXT,
    required_ack        BOOLEAN,
    cooling_off_seconds INT,
    step_up_method      TEXT
) AS $$
DECLARE
    v_band     TEXT;
    v_reasons  TEXT[];
    v_rule     warning_rules%ROWTYPE;
    v_matched  BOOLEAN;
    v_first    BOOLEAN;
    v_stepup   BOOLEAN;
    v_decision TEXT;
    v_method   TEXT := '';
BEGIN
    PERFORM assert_caller_owns(p_caller, p_debit);

    SELECT r.risk_band, r.reasons INTO v_band, v_reasons
    FROM assess_transfer_risk(p_caller, p_debit, p_credit, p_amount_minor, p_exclude_transfer) r;

    SELECT * INTO v_rule
    FROM warning_rules wr
    WHERE wr.active
      AND (wr.match_reason_code IS NULL OR wr.match_reason_code = ANY(v_reasons))
      AND (wr.match_min_band IS NULL OR
           (CASE v_band          WHEN 'high' THEN 3 WHEN 'medium' THEN 2 ELSE 1 END) >=
           (CASE wr.match_min_band WHEN 'high' THEN 3 WHEN 'medium' THEN 2 ELSE 1 END))
    ORDER BY (CASE wr.decision WHEN 'block' THEN 3 WHEN 'review' THEN 2 ELSE 1 END) DESC,
             wr.priority DESC, wr.created_at ASC
    LIMIT 1;
    v_matched := FOUND;

    -- "New payee" is the shared is_known_payee predicate — the SAME definition
    -- the Go gate reads via sqlc, NOT assess_transfer_risk's
    -- first_payment_to_payee — else the preview under/over-promises step-up.
    v_first := NOT is_known_payee(p_caller, p_credit);
    v_stepup := (p_step_up_limit_minor > 0 AND p_amount_minor >= p_step_up_limit_minor)
                OR v_band = 'high' OR v_first;
    IF v_stepup THEN v_method := 'otp'; END IF;

    IF v_matched AND v_rule.decision = 'block' THEN
        v_decision := 'block';
    ELSIF v_matched AND v_rule.decision = 'review' THEN
        v_decision := 'review';
    ELSIF v_stepup THEN
        v_decision := 'step_up';
    ELSIF v_matched AND v_rule.decision = 'warn' THEN
        v_decision := 'warn';
    ELSE
        v_decision := 'allow';
    END IF;

    RETURN QUERY SELECT
        v_decision, v_band, v_reasons,
        CASE WHEN v_matched THEN v_rule.id END,
        COALESCE(v_rule.category, ''), COALESCE(v_rule.headline, ''),
        COALESCE(v_rule.body, ''), COALESCE(v_rule.severity, ''),
        COALESCE(v_rule.required_ack, FALSE), COALESCE(v_rule.cooling_off_seconds, 0),
        v_method;
END;
$$ LANGUAGE plpgsql STABLE;

-- +goose StatementEnd

-- +goose Down
DROP TRIGGER IF EXISTS trg_warning_rules_updated_at ON warning_rules;
-- +goose StatementBegin
DROP FUNCTION IF EXISTS evaluate_transfer(UUID, UUID, UUID, BIGINT, BIGINT, UUID);
DROP FUNCTION IF EXISTS assert_warning_ack(UUID, TEXT, UUID, UUID, BIGINT, INT);
DROP FUNCTION IF EXISTS screen_payment(UUID, UUID);
-- +goose StatementEnd
DROP TABLE IF EXISTS watchlist_entries;
DROP TABLE IF EXISTS warning_rules;
DROP TRIGGER IF EXISTS trg_warning_acks_immutable ON warning_acks;
-- +goose StatementBegin
DROP FUNCTION IF EXISTS warning_acks_block_mutation();
DROP FUNCTION IF EXISTS record_warning_ack(UUID, TEXT, TEXT, BOOLEAN, UUID, TEXT, BIGINT, TEXT);
-- +goose StatementEnd
DROP TABLE IF EXISTS warning_acks;
-- +goose StatementBegin
DROP FUNCTION IF EXISTS assess_transfer_risk(UUID, UUID, UUID, BIGINT, UUID);
-- +goose StatementEnd
