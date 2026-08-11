-- name: GetCommerceActorForShare :one
SELECT u.id, u.role, u.status, a.currency
FROM users AS u
JOIN accounts AS a ON a.user_id = u.id
WHERE u.id = $1
FOR SHARE OF u, a;

-- name: GetProductForOrder :one
SELECT id, sku, name, price_amount, currency, status, version
FROM products
WHERE id = $1
FOR SHARE;

-- name: DeductInventoryForOrder :one
UPDATE inventories
SET quantity = quantity - $2,
    version = version + 1,
    updated_at = $3
WHERE product_id = $1
  AND quantity >= $2
RETURNING product_id, quantity, version, created_at, updated_at;

-- name: CreateOrder :one
INSERT INTO orders (id, user_id, status, currency, total_amount, created_at, updated_at)
VALUES ($1, $2, 'pending', $3, $4, $5, $5)
RETURNING id, user_id, status, currency, total_amount, created_at, updated_at;

-- name: CreateOrderItem :one
INSERT INTO order_items (
    id, order_id, product_id, product_sku, product_name, product_version,
    quantity, unit_price_amount, line_amount, created_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10
)
RETURNING id, order_id, product_id, product_sku, product_name, product_version,
          quantity, unit_price_amount, line_amount, created_at;

-- name: GetOrderByID :one
SELECT id, user_id, status, currency, total_amount, created_at, updated_at
FROM orders
WHERE id = $1;

-- name: GetOrderByIDAndUser :one
SELECT id, user_id, status, currency, total_amount, created_at, updated_at
FROM orders
WHERE id = $1 AND user_id = $2;

-- name: ListOrdersByUser :many
SELECT id, user_id, status, currency, total_amount, created_at, updated_at
FROM orders
WHERE user_id = $1
ORDER BY created_at DESC, id DESC
LIMIT $2;

-- name: ListOrdersByUserAfter :many
SELECT id, user_id, status, currency, total_amount, created_at, updated_at
FROM orders
WHERE user_id = $1
  AND (created_at, id) < (
      sqlc.arg(cursor_created_at)::timestamptz,
      sqlc.arg(cursor_id)::uuid
  )
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(result_limit);

-- name: ListAllOrders :many
SELECT id, user_id, status, currency, total_amount, created_at, updated_at
FROM orders
ORDER BY created_at DESC, id DESC
LIMIT $1;

-- name: ListAllOrdersAfter :many
SELECT id, user_id, status, currency, total_amount, created_at, updated_at
FROM orders
WHERE (created_at, id) < (
    sqlc.arg(cursor_created_at)::timestamptz,
    sqlc.arg(cursor_id)::uuid
)
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(result_limit);

-- name: ListOrderItemsByOrderIDs :many
SELECT id, order_id, product_id, product_sku, product_name, product_version,
       quantity, unit_price_amount, line_amount, created_at
FROM order_items
WHERE order_id = ANY($1::uuid[])
ORDER BY order_id, id;

-- name: ListOrderItemsByOrderID :many
SELECT id, order_id, product_id, product_sku, product_name, product_version,
       quantity, unit_price_amount, line_amount, created_at
FROM order_items
WHERE order_id = $1
ORDER BY product_id;
