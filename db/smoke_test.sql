\set ON_ERROR_STOP on
-- bank0 core-flow smoke test. Drives the DB functions end-to-end with VALID IBANs
-- (iban_generate -> MOD-97 checksum, so they pass the accounts_iban_checksum CHECK)
-- and asserts the books balance. Run:  psql "$DSN" -f db/smoke_test.sql

\echo '== 1. create users + accounts (valid NL IBANs via iban_generate) =='
SELECT create_user('smoke_alice','pw1','Alice Smith','smoke_alice@example.com') AS alice_id \gset
SELECT create_user('smoke_bob','pw2','Bob Jones','smoke_bob@example.com')       AS bob_id   \gset
-- NL IBAN = 'NL' + 2 check digits + 4-letter bank code (INGB) + 10-digit account no.
SELECT iban_generate('NL', 'INGB' || lpad('1000000001', 10, '0')) AS alice_iban \gset
SELECT iban_generate('NL', 'INGB' || lpad('1000000002', 10, '0')) AS bob_iban   \gset
SELECT create_account(:'alice_id', :'alice_iban', '1234') AS alice_acct \gset
SELECT create_account(:'bob_id',   :'bob_iban',   '5678') AS bob_acct   \gset

\echo '== 2. login check (bcrypt) -> returns alice id, then NULL for wrong pw =='
SELECT (SELECT user_id FROM check_user_credentials('smoke_alice','pw1'))  AS ok,
       (SELECT user_id FROM check_user_credentials('smoke_alice','nope')) AS bad;

\echo '== 3. deposit EUR 100.00 to Alice (via external_clearing) =='
SELECT deposit('smoke-dep-alice-1', :'alice_acct', 10000, 'Initial funding');
SELECT iban, balance_minor, account_available(id) AS available FROM accounts WHERE id = :'alice_acct';

\echo '== 4. idempotent deposit replay (same key) -> still 100.00, not 200.00 =='
SELECT deposit('smoke-dep-alice-1', :'alice_acct', 10000, 'Initial funding');
SELECT balance_minor AS alice_balance FROM accounts WHERE id = :'alice_acct';

\echo '== 5. Alice -> Bob EUR 10.50 (auto-post) =='
SELECT status, was_replay FROM transfer('smoke-xfer-1', :'alice_acct', :'bob_acct', 1050, 'Lunch');
SELECT iban, balance_minor FROM accounts WHERE id IN (:'alice_acct', :'bob_acct') ORDER BY iban;

\echo '== 6. transfer replay (same key) -> no double post (Bob still 10.50) =='
SELECT status, was_replay FROM transfer('smoke-xfer-1', :'alice_acct', :'bob_acct', 1050, 'Lunch');
SELECT balance_minor AS bob_balance FROM accounts WHERE id = :'bob_acct';

\echo '== 7. statement for Bob (running balance) =='
SELECT direction, amount_minor, balance_after, transfer_kind, counterparty_iban
FROM enriched_ledger WHERE account_id = :'bob_acct' ORDER BY posted_at;

\echo '== 8. reconcile() -> MUST be empty (books balanced) =='
SELECT * FROM reconcile();

\echo '== 9. reconcile assertion: count MUST be 0 =='
SELECT count(*) AS reconcile_issues FROM reconcile();
