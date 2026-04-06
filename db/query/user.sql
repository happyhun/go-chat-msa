-- name: CreateUser :one
INSERT INTO users (
  id, username, password_hash, created_at
) VALUES (
  $1, $2, $3, $4
) RETURNING id, username, created_at;

-- name: GetUserByUsername :one
SELECT * FROM users
WHERE username = $1 LIMIT 1;

-- name: GetUserByID :one
SELECT * FROM users
WHERE id = $1 LIMIT 1;
