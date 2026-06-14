-- +goose Up
-- +goose StatementBegin

-- Operator-configurable console page size (default 15), tweakable from the Settings
-- panel like the other bank_settings policy knobs. Bounded so a typo can't ask for a
-- 0-row or absurd page.
ALTER TABLE bank_settings
    ADD COLUMN default_page_limit INT NOT NULL DEFAULT 15 CHECK (default_page_limit BETWEEN 1 AND 200);

-- Extend update_bank_settings with the page size (signature change -> drop + recreate).
DROP FUNCTION IF EXISTS update_bank_settings(BIGINT, BIGINT, UUID);
CREATE FUNCTION update_bank_settings(
    p_threshold_minor     BIGINT,
    p_default_limit_minor BIGINT,
    p_page_limit          INT,
    p_actor               UUID
) RETURNS VOID AS $$
    UPDATE bank_settings
       SET maker_checker_threshold_minor = p_threshold_minor,
           default_transfer_limit_minor  = p_default_limit_minor,
           default_page_limit            = p_page_limit,
           updated_at = now(), updated_by = p_actor
     WHERE id;
$$ LANGUAGE sql;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP FUNCTION IF EXISTS update_bank_settings(BIGINT, BIGINT, INT, UUID);
CREATE FUNCTION update_bank_settings(
    p_threshold_minor     BIGINT,
    p_default_limit_minor BIGINT,
    p_actor               UUID
) RETURNS VOID AS $$
    UPDATE bank_settings
       SET maker_checker_threshold_minor = p_threshold_minor,
           default_transfer_limit_minor  = p_default_limit_minor,
           updated_at = now(), updated_by = p_actor
     WHERE id;
$$ LANGUAGE sql;
-- +goose StatementEnd
ALTER TABLE bank_settings DROP COLUMN IF EXISTS default_page_limit;
