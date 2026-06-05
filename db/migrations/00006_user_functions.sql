-- +goose Up
-- +goose StatementBegin

-- create_user: passwords are hashed with bcrypt (pgcrypto). Never plaintext.
CREATE OR REPLACE FUNCTION create_user(
    p_username     CITEXT,
    p_password     TEXT,
    p_full_name    TEXT,
    p_email        CITEXT      DEFAULT NULL,
    p_phone_number VARCHAR(16) DEFAULT NULL,
    p_role         user_role   DEFAULT 'customer'
) RETURNS UUID AS $$
DECLARE
    v_user_id UUID;
BEGIN
    INSERT INTO users (username, password_hash, full_name, email, phone_number, role)
    VALUES (
        p_username,
        crypt(p_password, gen_salt('bf', 10)),
        p_full_name,
        NULLIF(p_email, ''),          -- store NULL (not '') so unique index ignores it
        NULLIF(p_phone_number, ''),
        p_role
    )
    RETURNING id INTO v_user_id;
    RETURN v_user_id;
END;
$$ LANGUAGE plpgsql;

-- check_user_credentials: bcrypt verify (constant-time-ish via crypt()).
CREATE OR REPLACE FUNCTION check_user_credentials(
    p_username CITEXT,
    p_password TEXT
) RETURNS UUID AS $$
DECLARE
    v_id UUID;
BEGIN
    SELECT id INTO v_id
    FROM users
    WHERE username = p_username
      AND status = 'active'
      AND password_hash = crypt(p_password, password_hash);
    RETURN v_id;   -- NULL when invalid; caller maps to 401
END;
$$ LANGUAGE plpgsql;

-- update_user_info: partial update; re-hashes password only when provided.
CREATE OR REPLACE FUNCTION update_user_info(
    p_user_id      UUID,
    p_full_name    TEXT        DEFAULT NULL,
    p_email        CITEXT      DEFAULT NULL,
    p_phone_number VARCHAR(16) DEFAULT NULL,
    p_password     TEXT        DEFAULT NULL,
    p_status       user_status DEFAULT NULL
) RETURNS VOID AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM users WHERE id = p_user_id) THEN
        RAISE EXCEPTION 'user % does not exist', p_user_id;
    END IF;

    UPDATE users SET
        full_name     = COALESCE(p_full_name, full_name),
        email         = COALESCE(NULLIF(p_email, ''), email),
        phone_number  = COALESCE(NULLIF(p_phone_number, ''), phone_number),
        password_hash = CASE WHEN p_password IS NOT NULL AND p_password <> ''
                             THEN crypt(p_password, gen_salt('bf', 10))
                             ELSE password_hash END,
        status        = COALESCE(p_status, status)
    WHERE id = p_user_id;
END;
$$ LANGUAGE plpgsql;

-- +goose StatementEnd

-- +goose Down
DROP FUNCTION IF EXISTS update_user_info(UUID, TEXT, CITEXT, VARCHAR, TEXT, user_status);
DROP FUNCTION IF EXISTS check_user_credentials(CITEXT, TEXT);
DROP FUNCTION IF EXISTS create_user(CITEXT, TEXT, TEXT, CITEXT, VARCHAR, user_role);
