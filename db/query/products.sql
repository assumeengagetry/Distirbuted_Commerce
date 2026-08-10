-- name: CreateProduct :one
INSERT INTO products (
    id, sku, name, description, price_amount, currency, status, version, created_at, updated_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, 1, $8, $8
)
RETURNING id, sku, name, description, price_amount, currency, status, version, created_at, updated_at;

-- name: CreateInventory :one
INSERT INTO inventories (product_id, quantity, version, created_at, updated_at)
VALUES ($1, $2, 1, $3, $3)
RETURNING product_id, quantity, version, created_at, updated_at;

-- name: GetActiveProductByID :one
SELECT
    p.id, p.sku, p.name, p.description, p.price_amount, p.currency,
    p.status, p.version, p.created_at, p.updated_at,
    i.quantity, i.version AS inventory_version, i.updated_at AS inventory_updated_at
FROM products AS p
JOIN inventories AS i ON i.product_id = p.id
WHERE p.id = $1 AND p.status = 'active';

-- name: GetProductByID :one
SELECT
    p.id, p.sku, p.name, p.description, p.price_amount, p.currency,
    p.status, p.version, p.created_at, p.updated_at,
    i.quantity, i.version AS inventory_version, i.updated_at AS inventory_updated_at
FROM products AS p
JOIN inventories AS i ON i.product_id = p.id
WHERE p.id = $1;

-- name: GetProductForUpdate :one
SELECT
    p.id, p.sku, p.name, p.description, p.price_amount, p.currency,
    p.status, p.version, p.created_at, p.updated_at
FROM products AS p
WHERE p.id = $1
FOR UPDATE OF p;

-- name: UpdateProduct :one
UPDATE products
SET name = $2,
    description = $3,
    price_amount = $4,
    status = $5,
    version = version + 1,
    updated_at = $6
WHERE id = $1 AND version = $7
RETURNING id, sku, name, description, price_amount, currency, status, version, created_at, updated_at;

-- name: GetInventoryForUpdate :one
SELECT
    i.product_id, i.quantity, i.version, i.created_at, i.updated_at,
    p.sku, p.name, p.status
FROM inventories AS i
JOIN products AS p ON p.id = i.product_id
WHERE i.product_id = $1
FOR UPDATE OF i;

-- name: UpdateInventory :one
UPDATE inventories
SET quantity = $2,
    version = version + 1,
    updated_at = $3
WHERE product_id = $1 AND version = $4
RETURNING product_id, quantity, version, created_at, updated_at;

-- name: ListActiveProducts :many
SELECT
    p.id, p.sku, p.name, p.description, p.price_amount, p.currency,
    p.status, p.version, p.created_at, p.updated_at,
    i.quantity, i.version AS inventory_version, i.updated_at AS inventory_updated_at
FROM products AS p
JOIN inventories AS i ON i.product_id = p.id
WHERE p.status = 'active'
ORDER BY p.created_at DESC, p.id DESC
LIMIT $1;

-- name: ListActiveProductsAfter :many
SELECT
    p.id, p.sku, p.name, p.description, p.price_amount, p.currency,
    p.status, p.version, p.created_at, p.updated_at,
    i.quantity, i.version AS inventory_version, i.updated_at AS inventory_updated_at
FROM products AS p
JOIN inventories AS i ON i.product_id = p.id
WHERE p.status = 'active'
  AND (p.created_at, p.id) < (
      sqlc.arg(cursor_created_at)::timestamptz,
      sqlc.arg(cursor_id)::uuid
  )
ORDER BY p.created_at DESC, p.id DESC
LIMIT sqlc.arg(result_limit);

-- name: ListInventory :many
SELECT
    p.id AS product_id, p.sku, p.name, p.status, p.created_at AS product_created_at,
    i.quantity, i.version, i.updated_at
FROM products AS p
JOIN inventories AS i ON i.product_id = p.id
ORDER BY p.created_at DESC, p.id DESC
LIMIT $1;

-- name: ListInventoryAfter :many
SELECT
    p.id AS product_id, p.sku, p.name, p.status, p.created_at AS product_created_at,
    i.quantity, i.version, i.updated_at
FROM products AS p
JOIN inventories AS i ON i.product_id = p.id
WHERE (p.created_at, p.id) < (
    sqlc.arg(cursor_created_at)::timestamptz,
    sqlc.arg(cursor_id)::uuid
)
ORDER BY p.created_at DESC, p.id DESC
LIMIT sqlc.arg(result_limit);
