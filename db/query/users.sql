-- name: CreateUser :one
INSERT INTO users (
    id,
    email,
    password_hash,
    display_name,
    role,
    status,
    created_at,
    updated_at
) VALUES (
    $1, $2, $3, $4, $5, 'active', $6, $6
)
RETURNING id, email, password_hash, display_name, role, status, created_at, updated_at;

-- name: CreateAccount :one
INSERT INTO accounts (
    id,
    user_id,
    currency,
    balance,
    created_at,
    updated_at
) VALUES (
    $1, $2, $3, 0, $4, $4
)
RETURNING id, user_id, currency, balance, created_at, updated_at;

-- name: GetUserCredentialsByEmail :one
SELECT
    u.id,
    u.email,
    u.password_hash,
    u.display_name,
    u.role,
    u.status,
    u.created_at,
    u.updated_at,
    a.id AS account_id,
    a.currency,
    a.balance,
    a.created_at AS account_created_at,
    a.updated_at AS account_updated_at
FROM users AS u
JOIN accounts AS a ON a.user_id = u.id
WHERE u.email = $1;

-- name: GetUserProfileByID :one
SELECT
    u.id,
    u.email,
    u.display_name,
    u.role,
    u.status,
    u.created_at,
    u.updated_at,
    a.id AS account_id,
    a.currency,
    a.balance,
    a.created_at AS account_created_at,
    a.updated_at AS account_updated_at
FROM users AS u
JOIN accounts AS a ON a.user_id = u.id
WHERE u.id = $1;
