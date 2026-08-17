-- name: CreateAuthSession :one
INSERT INTO auth_sessions (
    id,
    user_id,
    expires_at,
    created_at,
    updated_at
) VALUES (
    $1, $2, $3, $4, $4
)
RETURNING id, user_id, expires_at, revoked_at, created_at, updated_at;

-- name: CreateRefreshToken :one
INSERT INTO refresh_tokens (
    id,
    session_id,
    token_hash,
    created_at
) VALUES (
    $1, $2, $3, $4
)
RETURNING id, session_id, token_hash, created_at, consumed_at;

-- name: GetRefreshTokenStateForUpdate :one
SELECT
    rt.id AS refresh_token_id,
    rt.token_hash,
    rt.created_at AS refresh_created_at,
    rt.consumed_at,
    s.id AS session_id,
    s.expires_at AS session_expires_at,
    s.revoked_at AS session_revoked_at,
    u.id AS user_id,
    u.email,
    u.display_name,
    u.role,
    u.status,
    u.created_at AS user_created_at,
    u.updated_at AS user_updated_at,
    a.id AS account_id,
    a.currency,
    a.balance,
    a.created_at AS account_created_at,
    a.updated_at AS account_updated_at
FROM refresh_tokens AS rt
JOIN auth_sessions AS s ON s.id = rt.session_id
JOIN users AS u ON u.id = s.user_id
JOIN accounts AS a ON a.user_id = u.id
WHERE rt.id = $1
FOR UPDATE OF s, rt;

-- name: ConsumeRefreshToken :execrows
UPDATE refresh_tokens
SET consumed_at = $2
WHERE id = $1
  AND consumed_at IS NULL;

-- name: RevokeAuthSession :exec
UPDATE auth_sessions
SET revoked_at = COALESCE(revoked_at, $2),
    updated_at = $2
WHERE id = $1;

-- name: DeleteExpiredAuthSessions :execrows
DELETE FROM auth_sessions AS target
WHERE target.id IN (
    SELECT expired.id
    FROM auth_sessions AS expired
    WHERE expired.expires_at <= CURRENT_TIMESTAMP
    ORDER BY expired.expires_at
    LIMIT $1
    FOR UPDATE SKIP LOCKED
);
