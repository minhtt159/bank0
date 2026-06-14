\set ON_ERROR_STOP on
\echo '== 1. create users + accounts =='
SELECT create_user('alice','pw1','Alice Smith','alice@example.com') AS alice_id \gset
SELECT create_user('bob','pw2','Bob Jones','bob@example.com')       AS bob_id   \gset
SELECT create_account(:'alice_id','DE00ALICE00000001','1234') AS alice_acct \gset
SELECT create_account(:'bob_id',  'DE00BOB0000000001','5678') AS bob_acct   \gset

\echo '== 2. login check (bcrypt) -> returns alice id, then NULL for wrong pw =='
SELECT (SELECT user_id FROM check_user_credentials('alice','pw1'))  AS ok,
       (SELECT user_id FROM check_user_credentials('alice','nope')) AS bad;

\echo '== 3. deposit EUR 100.00 to Alice (via external_clearing) =='
SELECT deposit('dep-alice-1', :'alice_acct', 10000, 'Initial funding');
SELECT iban, balance_minor, account_available(id) AS available FROM accounts WHERE id = :'alice_acct';

\echo '== 4. idempotent deposit replay (same key) -> still 100.00, not 200.00 =='
SELECT deposit('dep-alice-1', :'alice_acct', 10000, 'Initial funding');
SELECT balance_minor FROM accounts WHERE id = :'alice_acct';

\echo '== 5. Alice -> Bob EUR 10.50 (auto-post) =='
SELECT status, was_replay FROM transfer('xfer-1', :'alice_acct', :'bob_acct', 1050, 'Lunch');
SELECT iban, balance_minor FROM accounts WHERE id IN (:'alice_acct', :'bob_acct') ORDER BY iban;

\echo '== 6. transfer replay (same key) -> no double post (Bob still 10.50) =='
SELECT status, was_replay FROM transfer('xfer-1', :'alice_acct', :'bob_acct', 1050, 'Lunch');
SELECT balance_minor AS bob_balance FROM accounts WHERE id = :'bob_acct';

\echo '== 7. statement for Bob (running balance) =='
SELECT direction, amount_minor, balance_after, transfer_kind, counterparty_iban
FROM enriched_ledger WHERE account_id = :'bob_acct' ORDER BY posted_at;

\echo '== 8. reverse the Alice->Bob transfer =='
SELECT id AS xfer_id FROM transfers WHERE kind='transfer' AND status='posted' LIMIT 1 \gset
SELECT reverse_transfer(:'xfer_id', 'rev-1', 'wrong recipient') AS reversal_id;
SELECT iban, balance_minor FROM accounts WHERE id IN (:'alice_acct', :'bob_acct') ORDER BY iban;
SELECT status FROM transfers WHERE id = :'xfer_id';

\echo '== 9. reconcile() -> MUST be empty (books balanced) =='
SELECT * FROM reconcile();

\echo '== 10. global zero-sum check: SUM(signed_amount) over ALL entries =='
SELECT COALESCE(SUM(signed_amount),0) AS global_sum FROM ledger_entries;
