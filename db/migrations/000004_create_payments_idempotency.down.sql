DROP TABLE IF EXISTS payments;
DROP TABLE IF EXISTS idempotency_keys;
DROP FUNCTION IF EXISTS enforce_payment_settlement();
DROP FUNCTION IF EXISTS enforce_payment_immutable();

DROP TRIGGER IF EXISTS orders_paid_status_terminal ON orders;
DROP FUNCTION IF EXISTS enforce_paid_order_terminal();

DROP TRIGGER IF EXISTS accounts_record_balance_entry ON accounts;
DROP FUNCTION IF EXISTS record_account_balance_entry();
DROP TRIGGER IF EXISTS accounts_version_balance ON accounts;
DROP FUNCTION IF EXISTS version_account_balance();
DROP TABLE IF EXISTS account_balance_entries;

ALTER TABLE orders DROP CONSTRAINT IF EXISTS orders_payment_identity_unique;
ALTER TABLE orders DROP CONSTRAINT IF EXISTS orders_id_user_unique;
ALTER TABLE accounts DROP CONSTRAINT IF EXISTS accounts_payment_identity_unique;
ALTER TABLE accounts DROP CONSTRAINT IF EXISTS accounts_id_user_unique;
ALTER TABLE accounts DROP CONSTRAINT IF EXISTS accounts_balance_version_valid;
ALTER TABLE accounts DROP COLUMN IF EXISTS balance_version;
