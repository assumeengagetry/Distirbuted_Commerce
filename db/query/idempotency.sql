-- name: ReserveIdempotencyKey :one
INSERT INTO idempotency_keys (
    actor_id, operation, key_hash, request_hash, resource_id,
    state, response_status, created_at, completed_at
) VALUES (
    $1, $2, $3, $4, $5, 'reserved', NULL, clock_timestamp(), NULL
)
ON CONFLICT (actor_id, operation, key_hash) DO NOTHING
RETURNING actor_id, operation, key_hash, request_hash, resource_id,
          state, response_status, created_at, completed_at;

-- name: GetIdempotencyKeyForUpdate :one
SELECT actor_id, operation, key_hash, request_hash, resource_id,
       state, response_status, created_at, completed_at
FROM idempotency_keys
WHERE actor_id = $1 AND operation = $2 AND key_hash = $3
FOR UPDATE;

-- name: GetIdempotencyKey :one
SELECT actor_id, operation, key_hash, request_hash, resource_id,
       state, response_status, created_at, completed_at
FROM idempotency_keys
WHERE actor_id = $1 AND operation = $2 AND key_hash = $3;

-- name: CompleteIdempotencyKey :one
UPDATE idempotency_keys
SET state = 'completed',
    response_status = $6,
    completed_at = clock_timestamp()
WHERE actor_id = $1
  AND operation = $2
  AND key_hash = $3
  AND request_hash = $4
  AND resource_id = $5
  AND state = 'reserved'
RETURNING actor_id, operation, key_hash, request_hash, resource_id,
          state, response_status, created_at, completed_at;
