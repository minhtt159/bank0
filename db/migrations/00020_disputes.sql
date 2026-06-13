-- +goose Up
-- +goose StatementBegin

CREATE TYPE dispute_status   AS ENUM ('open', 'under_review', 'resolved', 'rejected');
CREATE TYPE dispute_category AS ENUM ('unrecognised', 'fraud', 'wrong_amount', 'duplicate', 'other');

-- disputes: a customer "I don't recognise this" case against a transfer they are a
-- party to. NOT money state — the ledger is append-only; remedy stays operator-side
-- (reverse_transfer, 00008). Only this row's status/resolution fields mutate.
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

-- updated_at maintenance: reuse the project's shared trigger fn from 00005.
CREATE TRIGGER trg_disputes_updated_at
  BEFORE UPDATE ON disputes
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ─────────────────────────────────────────────────────────────────────────────
-- raise_dispute: open a case. Caller must be a party to the transfer (debit or
-- credit owner). Records a fraud signal in admin_actions (the server-side hook —
-- flag-only, no auto-freeze). The partial unique index enforces "no duplicate open
-- dispute" (23505 -> 409).
--   not a party / unknown transfer -> P0001 "not found" -> 404 (existence hidden)
--   transfer not settled            -> check_violation 23514 -> 422
-- ─────────────────────────────────────────────────────────────────────────────
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

-- ─────────────────────────────────────────────────────────────────────────────
-- resolve_dispute: operator transition (state machine). Records the resolver +
-- note; appends an admin_actions audit row. Illegal transitions raise a plain
-- "cannot ..." exception (P0001) which mapDBError maps to 409 invalid_state;
-- unknown id -> P0001 "not found" -> 404.
--   open          -> under_review | resolved | rejected
--   under_review  -> resolved | rejected   (under_review->under_review is a no-op)
--   resolved/rejected -> (terminal) -> 409
-- ─────────────────────────────────────────────────────────────────────────────
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
DROP TABLE IF EXISTS disputes;
DROP TYPE IF EXISTS dispute_category;
DROP TYPE IF EXISTS dispute_status;
-- +goose StatementEnd
