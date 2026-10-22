BEGIN;

DROP TABLE outbox_events;
DROP TABLE wallet_ledger_entries;
DROP TABLE wager_transactions;
DROP TABLE wallets;
DROP FUNCTION check_wager_financial_records();
DROP FUNCTION guard_outbox_snapshot();
DROP FUNCTION financial_decimal(BIGINT);
DROP FUNCTION check_wallet_financial_state();
DROP FUNCTION guard_ledger_insert();
DROP FUNCTION guard_wager_state();
DROP FUNCTION guard_wallet_state();
DROP FUNCTION deny_financial_mutation();
DROP FUNCTION financial_identifier(TEXT);

COMMIT;
