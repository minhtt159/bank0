-- Unified list+search queries. When q is NULL/'' they return everything (the
-- plain list); otherwise they fuzzy-match (substring ILIKE OR trigram
-- word_similarity > 0.3), ranked by similarity.

-- name: SearchUsers :many
SELECT id, username, full_name, email, phone_number, role, status, created_at, updated_at
FROM users
WHERE sqlc.narg(q)::text IS NULL OR sqlc.narg(q)::text = ''
   OR username::text ILIKE '%' || sqlc.narg(q) || '%'
   OR full_name ILIKE '%' || sqlc.narg(q) || '%'
   OR COALESCE(email::text, '') ILIKE '%' || sqlc.narg(q) || '%'
   OR word_similarity(sqlc.narg(q)::text, username::text) > 0.3
   OR word_similarity(sqlc.narg(q)::text, full_name) > 0.3
ORDER BY GREATEST(
           word_similarity(COALESCE(sqlc.narg(q)::text, ''), username::text),
           word_similarity(COALESCE(sqlc.narg(q)::text, ''), full_name)
         ) DESC, created_at DESC
LIMIT sqlc.arg(page_limit)::int;

-- name: SearchAccounts :many
SELECT a.id, a.user_id,
       COALESCE(u.full_name, '') AS owner,
       COALESCE(a.iban, '')      AS iban,
       a.status, a.balance_minor,
       account_available(a.id)   AS available_minor
FROM accounts a
LEFT JOIN users u ON u.id = a.user_id
WHERE a.kind = 'customer' AND (
      sqlc.narg(q)::text IS NULL OR sqlc.narg(q)::text = ''
   OR a.iban::text ILIKE '%' || sqlc.narg(q) || '%'
   OR u.full_name ILIKE '%' || sqlc.narg(q) || '%'
   OR word_similarity(sqlc.narg(q)::text, COALESCE(a.iban::text, '')) > 0.3
   OR word_similarity(sqlc.narg(q)::text, COALESCE(u.full_name, '')) > 0.3
)
ORDER BY a.created_at DESC
LIMIT sqlc.arg(page_limit)::int;

-- name: SearchTransfers :many
SELECT t.id,
       COALESCE(da.iban, da.system_code, '') AS debit,
       COALESCE(ca.iban, ca.system_code, '') AS credit,
       t.kind, t.status, t.amount_minor, t.description, t.requested_at
FROM transfers t
JOIN accounts da ON da.id = t.debit_account_id
JOIN accounts ca ON ca.id = t.credit_account_id
WHERE sqlc.narg(q)::text IS NULL OR sqlc.narg(q)::text = ''
   OR t.description ILIKE '%' || sqlc.narg(q) || '%'
   OR COALESCE(da.iban::text, '') ILIKE '%' || sqlc.narg(q) || '%'
   OR COALESCE(ca.iban::text, '') ILIKE '%' || sqlc.narg(q) || '%'
   OR word_similarity(sqlc.narg(q)::text, t.description) > 0.3
ORDER BY t.requested_at DESC
LIMIT sqlc.arg(page_limit)::int;
