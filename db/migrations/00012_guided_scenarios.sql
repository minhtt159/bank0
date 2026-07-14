-- +goose Up
-- ─────────────────────────────────────────────────────────────────────────────
-- GUIDED SCENARIOS — fraudbank's "Guided transaction" APP-scam demo
-- The operator/seed-controlled mule-target config and the resolver behind
-- GET /transfers/suggestion. No money state. The guided_scenarios pool is also the
-- rule seam read by resolve_account_by_iban (00011) and assess_transfer_risk
-- (00015). Masking uses mask_name (00002).
-- ─────────────────────────────────────────────────────────────────────────────

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

-- +goose StatementBegin

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

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP FUNCTION IF EXISTS suggest_transfer_destinations(UUID, UUID, BIGINT);
-- +goose StatementEnd
DROP INDEX IF EXISTS idx_guided_scenarios_active;
DROP TABLE IF EXISTS guided_scenarios;
