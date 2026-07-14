-- +goose Up
-- ─────────────────────────────────────────────────────────────────────────────
-- events  (per-user notification feed — an append-only PROJECTION, not a second
-- source of truth for money; a lost event never corrupts the ledger)
-- Rows are written INSIDE the transaction that owns the source transition:
-- post_transfer (00008) emits transfer.posted / payment.incoming,
-- issue_refresh_token (00004) emits device.new, resolve_dispute (00013) emits
-- dispute.updated. The event and its cause commit or roll back together. Those
-- emitters are created BEFORE this table exists; they reference it via late-bound
-- plpgsql bodies (and emit_event here), so the CREATE order is dependency-clean.
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
    WHERE type IN ('transfer.posted', 'payment.incoming', 'transfer.held');
-- Idempotent DEVICE emission: one device.new per refresh-token family.
CREATE UNIQUE INDEX uq_events_device_family ON events ((data->>'family_id'))
    WHERE type = 'device.new';

-- +goose StatementBegin

-- events_block_mutation: the feed is a record of things that happened. Only
-- read_at may ever change; deletes are blocked (user-cascade is the sole removal).
-- Mirrors ledger_block_mutation (00008).
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
        WHERE type IN ('transfer.posted', 'payment.incoming', 'transfer.held')
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

-- +goose Down
DROP TRIGGER IF EXISTS trg_events_immutable ON events;
-- +goose StatementBegin
DROP FUNCTION IF EXISTS mark_events_read(UUID, TIMESTAMPTZ, UUID);
DROP FUNCTION IF EXISTS emit_event(UUID, event_type, TEXT, TEXT, UUID, UUID, JSONB);
DROP FUNCTION IF EXISTS events_block_mutation();
-- +goose StatementEnd
DROP TABLE IF EXISTS events;
