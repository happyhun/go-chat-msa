-- name: CreateUser :one
INSERT INTO users (
  id, username, password_hash, created_at
) VALUES (
  $1, $2, $3, $4
) RETURNING id, username, created_at;

-- name: GetUserByUsername :one
SELECT * FROM users
WHERE username = $1 AND deleted_at IS NULL LIMIT 1;

-- name: GetUserByID :one
SELECT * FROM users
WHERE id = $1 AND deleted_at IS NULL LIMIT 1;

-- name: SoftDeleteUser :one
UPDATE users
SET deleted_at = $2
WHERE id = $1 AND deleted_at IS NULL
RETURNING id;

-- name: PurgeDeletedUsers :execrows
DELETE FROM users
WHERE deleted_at IS NOT NULL
  AND deleted_at < $1;
