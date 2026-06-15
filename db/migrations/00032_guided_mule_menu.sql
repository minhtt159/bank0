-- +goose Up
-- +goose StatementBegin

-- Guided-transfer "mule menu" (v2, spec-guided-transfer-mule-menu.md). The endpoint
-- now returns a MENU of up to N candidate accounts belonging to OTHER users — a
-- random, shuffled pool of plausible payees that optionally includes a short-listed
-- mule (the active guided_scenarios target). The client picks one at random; an
-- empty result means "no stranger eligible — the client falls back to the caller's
-- own account". The own-account fallback that v1 did server-side moves to the client.
--
-- guided_scenarios (00019) is unchanged — it still holds the optional short-listed
-- mule. This migration supersedes the singular suggest_transfer_destination with the
-- plural suggest_transfer_destinations; the singular is dropped (Down restores it).
DROP FUNCTION IF EXISTS suggest_transfer_destination(UUID, UUID, BIGINT);

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

-- Restore the v1 singular resolver (verbatim from 00019).
CREATE OR REPLACE FUNCTION suggest_transfer_destination(
    p_caller       UUID,
    p_from_account UUID,
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
    IF v_from IS NULL THEN
        SELECT id INTO v_from FROM accounts
         WHERE user_id = p_caller AND kind = 'customer' AND is_default
         LIMIT 1;
    END IF;

    SELECT gs.* INTO v_scn
    FROM guided_scenarios gs
    JOIN accounts ta ON ta.id = gs.target_account_id
    WHERE gs.active
      AND COALESCE(p_amount_minor, 0) >= gs.min_amount_minor
      AND (gs.target_user_id IS NULL OR gs.target_user_id = p_caller)
      AND ta.kind = 'customer' AND ta.status = 'active'
      AND (v_from IS NULL OR ta.id <> v_from)
    ORDER BY (gs.target_user_id IS NOT NULL) DESC, gs.priority DESC, gs.created_at DESC
    LIMIT 1;

    IF FOUND THEN
        SELECT * INTO v_acct FROM accounts WHERE id = v_scn.target_account_id;
        RETURN QUERY SELECT v_acct.id, v_acct.iban,
                            mask_name((SELECT full_name FROM users WHERE id = v_acct.user_id)),
                            v_scn.reason, v_scn.name, 'scenario'::text;
        RETURN;
    END IF;

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

    RETURN;
END;
$$ LANGUAGE plpgsql STABLE;
-- +goose StatementEnd
