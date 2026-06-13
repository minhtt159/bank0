-- +goose Up
-- +goose StatementBegin

-- guided_scenarios: demo/config for fraudbank's "Guided transaction" mode (an
-- APP-scam simulation). A scenario maps an active demo to a target ("mule")
-- account that GET /transfers/suggestion will suggest. NO money state lives here.
-- Operator/seed-controlled; the client only toggles whether it ASKS for a
-- suggestion, never which account is returned. Empty by default => the endpoint
-- behaves exactly like the safe client stand-in it replaces.
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

-- suggest_transfer_destination: the resolver.
--   1. Try an active scenario matching this caller + amount (per-user beats
--      global; higher priority wins; ties broken by newest). The target must be an
--      active customer account and must differ from p_from_account.
--   2. Else fall back to the caller's own OTHER active customer account (the safe
--      default — same behaviour as the client stand-in being replaced).
-- Returns the masked owner name via mask_name() (00016) — never a full name or
-- balance. Returns zero rows when nothing is eligible (handler -> 204).
CREATE OR REPLACE FUNCTION suggest_transfer_destination(
    p_caller       UUID,
    p_from_account UUID,           -- may be NULL; resolver substitutes the caller's default account
    p_amount_minor BIGINT DEFAULT 0
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
    v_from   UUID := p_from_account;
    v_acct   accounts%ROWTYPE;
    v_scn    guided_scenarios%ROWTYPE;
BEGIN
    -- Resolve the effective debit account: explicit, else the caller's default.
    IF v_from IS NULL THEN
        SELECT id INTO v_from FROM accounts
         WHERE user_id = p_caller AND kind = 'customer' AND is_default
         LIMIT 1;
    END IF;

    -- (1) Scenario match. Per-user targeting beats global; priority then recency.
    SELECT gs.* INTO v_scn
    FROM guided_scenarios gs
    JOIN accounts ta ON ta.id = gs.target_account_id
    WHERE gs.active
      AND COALESCE(p_amount_minor, 0) >= gs.min_amount_minor
      AND (gs.target_user_id IS NULL OR gs.target_user_id = p_caller)
      AND ta.kind = 'customer' AND ta.status = 'active'
      AND (v_from IS NULL OR ta.id <> v_from)        -- never suggest the debit account
    ORDER BY (gs.target_user_id IS NOT NULL) DESC, gs.priority DESC, gs.created_at DESC
    LIMIT 1;

    IF FOUND THEN
        SELECT * INTO v_acct FROM accounts WHERE id = v_scn.target_account_id;
        RETURN QUERY SELECT v_acct.id, v_acct.iban,
                            mask_name((SELECT full_name FROM users WHERE id = v_acct.user_id)),
                            v_scn.reason, v_scn.name, 'scenario'::text;
        RETURN;
    END IF;

    -- (2) Safe default: the caller's own OTHER active customer account.
    SELECT a.* INTO v_acct
    FROM accounts a
    WHERE a.user_id = p_caller AND a.kind = 'customer' AND a.status = 'active'
      AND (v_from IS NULL OR a.id <> v_from)
    ORDER BY a.is_default DESC, a.created_at
    LIMIT 1;

    IF FOUND THEN
        RETURN QUERY SELECT v_acct.id, v_acct.iban,
                            mask_name((SELECT full_name FROM users WHERE id = v_acct.user_id)),
                            ('your ' || v_acct.kind || ' account')::text,
                            NULL::text, 'own_account'::text;
        RETURN;
    END IF;

    -- Nothing eligible -> empty result set -> handler returns 204.
    RETURN;
END;
$$ LANGUAGE plpgsql STABLE;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP FUNCTION IF EXISTS suggest_transfer_destination(UUID, UUID, BIGINT);
DROP INDEX IF EXISTS idx_guided_scenarios_active;
DROP TABLE IF EXISTS guided_scenarios;
-- +goose StatementEnd
