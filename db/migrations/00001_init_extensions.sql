-- +goose Up
-- pgcrypto: bcrypt password/PIN hashing (crypt/gen_salt) and gen_random_bytes.
-- citext:   case-insensitive username/email so 'Alice' == 'alice'.
CREATE EXTENSION IF NOT EXISTS pgcrypto;
CREATE EXTENSION IF NOT EXISTS citext;

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

-- +goose Down
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
DROP EXTENSION IF EXISTS citext;
DROP EXTENSION IF EXISTS pgcrypto;
