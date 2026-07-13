-- Warning/decision rules (warning_rules, 00008) — the console fraud-policy CRUD.
-- The rule engine itself lives in evaluate_transfer (hand-written wrapper in
-- internal/db/bank.go); these are the operator maintenance queries.

-- name: ListWarningRules :many
-- All rules (incl. inactive) for the console, best-first.
SELECT id, match_reason_code, match_min_band, category, headline, body, severity,
       decision, required_ack, cooling_off_seconds, priority, active, created_at, updated_at
FROM warning_rules
ORDER BY active DESC, priority DESC, created_at DESC;

-- name: CreateWarningRule :one
INSERT INTO warning_rules (match_reason_code, match_min_band, category, headline, body,
                           severity, decision, required_ack, cooling_off_seconds, priority, active)
VALUES (sqlc.narg(match_reason_code)::text, sqlc.narg(match_min_band)::text, sqlc.arg(category)::text,
        sqlc.arg(headline)::text, sqlc.arg(body)::text, sqlc.arg(severity)::text,
        sqlc.arg(decision)::text, sqlc.arg(required_ack)::boolean, sqlc.arg(cooling_off_seconds)::int,
        sqlc.arg(priority)::int, sqlc.arg(active)::boolean)
RETURNING id;

-- name: UpdateWarningRule :exec
UPDATE warning_rules
   SET match_reason_code   = sqlc.narg(match_reason_code)::text,
       match_min_band      = sqlc.narg(match_min_band)::text,
       category            = sqlc.arg(category)::text,
       headline            = sqlc.arg(headline)::text,
       body                = sqlc.arg(body)::text,
       severity            = sqlc.arg(severity)::text,
       decision            = sqlc.arg(decision)::text,
       required_ack        = sqlc.arg(required_ack)::boolean,
       cooling_off_seconds = sqlc.arg(cooling_off_seconds)::int,
       priority            = sqlc.arg(priority)::int,
       active              = sqlc.arg(active)::boolean
 WHERE id = sqlc.arg(id)::uuid;

-- name: SetWarningRuleActive :exec
UPDATE warning_rules SET active = sqlc.arg(active)::boolean WHERE id = sqlc.arg(id)::uuid;
