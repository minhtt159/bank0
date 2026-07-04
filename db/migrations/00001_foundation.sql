-- +goose Up
-- ─────────────────────────────────────────────────────────────────────────────
-- DATA MODEL — FOUNDATION
-- Shared primitives every other domain builds on: Postgres extensions, the
-- uuidv7() polyfill that lets `DEFAULT uuidv7()` work identically on PG17
-- (Supabase) and PG18+, and ALL enum types (account/user/transfer/ledger/hold/
-- idempotency lifecycles + the dispute taxonomy). No tables or functions live
-- here — just the type vocabulary the rest of the schema is written against.
-- ─────────────────────────────────────────────────────────────────────────────

-- pgcrypto: bcrypt password/PIN hashing (crypt/gen_salt) and gen_random_bytes.
-- citext:   case-insensitive username/email so 'Alice' == 'alice'.
CREATE EXTENSION IF NOT EXISTS pgcrypto;
CREATE EXTENSION IF NOT EXISTS citext;

-- pg_trgm: fuzzy (typo-tolerant) search support for users/accounts/transfers.
-- Substring ILIKE and word_similarity() both use the GIN indexes built in the
-- user-model / core-banking files below.
CREATE EXTENSION IF NOT EXISTS pg_trgm;

-- uuidv7(): built into PostgreSQL 18. On older servers (e.g. Supabase, currently
-- PG17) we install a pure-SQL polyfill so the schema's `DEFAULT uuidv7()` works
-- unchanged. The guard makes this a no-op on PG18+, where the built-in wins and
-- this function is never created — so the same migrations run on both. See
-- docs/08-deployment-cloud-run-supabase.md §1.1.
-- +goose StatementBegin
DO $$
BEGIN
    IF current_setting('server_version_num')::int < 180000 THEN
        -- Time-ordered v7 UUID: millisecond unix timestamp in the high 48 bits,
        -- random elsewhere, with the version (7) and variant (RFC 4122) bits set.
        -- gen_random_uuid() (built in since PG13) supplies the randomness.
        CREATE OR REPLACE FUNCTION uuidv7() RETURNS uuid
        LANGUAGE sql VOLATILE PARALLEL SAFE AS $f$
            SELECT encode(
                set_bit(
                    set_bit(
                        overlay(
                            uuid_send(gen_random_uuid())
                            PLACING substring(
                                int8send((extract(epoch FROM clock_timestamp()) * 1000)::bigint)
                                FROM 3
                            )
                            FROM 1 FOR 6
                        ),
                        52, 1
                    ),
                    53, 1
                ),
                'hex'
            )::uuid;
        $f$;
    END IF;
END
$$;
-- +goose StatementEnd

-- ─────────────────────────────────────────────────────────────────────────────
-- Enum types — the shared vocabulary for every domain below.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TYPE user_role       AS ENUM ('customer', 'operator', 'admin', 'auditor');
CREATE TYPE user_status     AS ENUM ('active', 'locked', 'closed');

-- Onboarding lifecycle for self-registered users, distinct from user_status
-- (which gates login). Admin-created users are born 'active'; only public
-- self-registration walks pending_verification -> verified.
CREATE TYPE onboarding_status    AS ENUM ('pending_verification', 'verified', 'active', 'rejected');
CREATE TYPE verification_channel AS ENUM ('email', 'phone');
CREATE TYPE verification_status  AS ENUM ('pending', 'verified', 'expired', 'canceled');

CREATE TYPE account_kind    AS ENUM ('customer', 'system');
CREATE TYPE account_status  AS ENUM ('active', 'frozen', 'closed');

CREATE TYPE transfer_status AS ENUM ('pending', 'posted', 'failed', 'canceled', 'reversed');
CREATE TYPE transfer_kind   AS ENUM ('transfer', 'deposit', 'withdrawal', 'reversal', 'fee', 'adjustment');

CREATE TYPE entry_direction AS ENUM ('debit', 'credit');
CREATE TYPE hold_status     AS ENUM ('active', 'captured', 'released', 'expired');
CREATE TYPE ik_status       AS ENUM ('in_progress', 'completed');

-- Dispute taxonomy (customer "I don't recognise this" cases; tables live in the
-- features file). Defined here with the rest of the type vocabulary.
CREATE TYPE dispute_status   AS ENUM ('open', 'under_review', 'resolved', 'rejected');
CREATE TYPE dispute_category AS ENUM ('unrecognised', 'fraud', 'wrong_amount', 'duplicate', 'other');

-- +goose Down
DROP TYPE IF EXISTS dispute_category;
DROP TYPE IF EXISTS dispute_status;
DROP TYPE IF EXISTS verification_status;
DROP TYPE IF EXISTS verification_channel;
DROP TYPE IF EXISTS onboarding_status;
DROP TYPE IF EXISTS ik_status;
DROP TYPE IF EXISTS hold_status;
DROP TYPE IF EXISTS entry_direction;
DROP TYPE IF EXISTS transfer_kind;
DROP TYPE IF EXISTS transfer_status;
DROP TYPE IF EXISTS account_status;
DROP TYPE IF EXISTS account_kind;
DROP TYPE IF EXISTS user_status;
DROP TYPE IF EXISTS user_role;

-- Drop the polyfill only where we created it; never touch the PG18 built-in.
-- +goose StatementBegin
DO $$
BEGIN
    IF current_setting('server_version_num')::int < 180000 THEN
        DROP FUNCTION IF EXISTS uuidv7();
    END IF;
END
$$;
-- +goose StatementEnd
DROP EXTENSION IF EXISTS pg_trgm;
DROP EXTENSION IF EXISTS citext;
DROP EXTENSION IF EXISTS pgcrypto;
