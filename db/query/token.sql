-- name: CreateRefreshToken :exec
INSERT INTO refresh_tokens (
  id, user_id, token_hash, expires_at, created_at
) VALUES (
  $1, $2, $3, $4, $5
);

-- name: GetRefreshTokenByHashForUpdate :one
SELECT * FROM refresh_tokens
WHERE token_hash = $1 LIMIT 1
FOR UPDATE;

-- name: MarkRefreshTokenUsed :exec
UPDATE refresh_tokens SET used = true
WHERE id = $1;

-- name: DeleteRefreshTokensByUserID :exec
DELETE FROM refresh_tokens
WHERE user_id = $1;

-- name: DeleteRefreshTokenByHash :exec
DELETE FROM refresh_tokens
WHERE token_hash = $1;

-- name: DeleteExpiredRefreshTokens :exec
DELETE FROM refresh_tokens
WHERE expires_at < NOW();
