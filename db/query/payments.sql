-- name: GetPaymentActorForShare :one
SELECT id, role, status
FROM users
WHERE id = $1
FOR SHARE;

-- name: GetOrderForPayment :one
SELECT id, user_id, status, currency, total_amount, created_at, updated_at
FROM orders
WHERE id = $1 AND user_id = $2
FOR UPDATE;

-- name: GetAccountForPayment :one
SELECT id, user_id, currency, balance, balance_version, created_at, updated_at
FROM accounts
WHERE user_id = $1
FOR UPDATE;

-- name: DebitAccountForPayment :one
UPDATE accounts
SET balance = balance - sqlc.arg(amount)::bigint,
    updated_at = sqlc.arg(updated_at)::timestamptz
WHERE id = sqlc.arg(account_id)::uuid
  AND user_id = sqlc.arg(user_id)::uuid
  AND currency = sqlc.arg(currency)::text
  AND balance = sqlc.arg(expected_balance)::bigint
  AND balance >= sqlc.arg(amount)::bigint
RETURNING id, user_id, currency, balance, balance_version, created_at, updated_at;

-- name: CreatePayment :one
INSERT INTO payments (
    id, order_id, user_id, account_id, account_balance_version, status, currency, amount,
    balance_before, balance_after, created_at
) VALUES (
    $1, $2, $3, $4, $5, 'succeeded', $6, $7, $8, $9, $10
)
RETURNING id, order_id, user_id, account_id, account_balance_version, status, currency, amount,
          balance_before, balance_after, created_at;

-- name: MarkOrderPaid :one
UPDATE orders
SET status = 'paid', updated_at = $3
WHERE id = $1 AND user_id = $2 AND status = 'pending'
RETURNING id, user_id, status, currency, total_amount, created_at, updated_at;

-- name: GetPaymentByID :one
SELECT id, order_id, user_id, account_id, account_balance_version, status, currency, amount,
       balance_before, balance_after, created_at
FROM payments
WHERE id = $1;

-- name: GetPaymentByIDAndUser :one
SELECT id, order_id, user_id, account_id, account_balance_version, status, currency, amount,
       balance_before, balance_after, created_at
FROM payments
WHERE id = $1 AND user_id = $2;
