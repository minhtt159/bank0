-- +goose Up
-- pgcrypto: bcrypt password/PIN hashing (crypt/gen_salt) and gen_random_bytes.
-- citext:   case-insensitive username/email so 'Alice' == 'alice'.
-- NOTE: uuidv7() is built into PostgreSQL 18 — no extension or helper needed.
CREATE EXTENSION IF NOT EXISTS pgcrypto;
CREATE EXTENSION IF NOT EXISTS citext;

-- +goose Down
DROP EXTENSION IF EXISTS citext;
DROP EXTENSION IF EXISTS pgcrypto;
