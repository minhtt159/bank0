-- +goose Up
-- ─────────────────────────────────────────────────────────────────────────────
-- DISPUTES — the customer "I don't recognise this" case + PSR/APP-scam claim machine
-- A dispute is NOT money state: the ledger is append-only and any remedy stays
-- operator-side (decide_dispute reimburses via a real EXTERNAL_CLEARING transfer,
-- 00008). Holds the disputes table (raise/resolve/decide/set-recall state) and its
-- functions. The dispute/decision/recall enums live in 00001; admin_actions (00009)
-- carries the audit rows; events (00014) carries the filer notifications (both via
-- late-bound plpgsql). trg_disputes_updated_at hangs on set_updated_at (00003).
-- assess_transfer_risk lives with the fraud seam (00015).
-- ─────────────────────────────────────────────────────────────────────────────

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
    -- currency rides along (Rec 19) so the handler needs no post-commit read-back:
    -- money may have MOVED by the time this returns — a second round-trip that
    -- failed would misreport a durably-posted reimbursement as an error.
) RETURNS TABLE (payout_minor BIGINT, currency CHAR(3)) AS $$
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
            IF NOT FOUND THEN RAISE EXCEPTION 'EXTERNAL_CLEARING system account missing (run seed)' USING ERRCODE = 'XX000'; END IF;
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

    RETURN QUERY SELECT v_payout, v_t.currency;
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

-- +goose Down
-- +goose StatementBegin
DROP FUNCTION IF EXISTS set_dispute_recall(UUID, UUID, recall_status, TEXT);
DROP FUNCTION IF EXISTS decide_dispute(UUID, UUID, dispute_decision, BIGINT, BOOLEAN, TEXT);
DROP FUNCTION IF EXISTS resolve_dispute(UUID, UUID, dispute_status, TEXT);
DROP FUNCTION IF EXISTS raise_dispute(UUID, UUID, dispute_category, TEXT, scam_type);
-- +goose StatementEnd
DROP TABLE IF EXISTS disputes;
