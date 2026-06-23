package user

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"go-chat-msa/internal/shared/auth"

	"github.com/redis/go-redis/v9"
)

type RefreshTokenRotationStatus int

const (
	RefreshTokenInvalid RefreshTokenRotationStatus = iota
	RefreshTokenRotated
	RefreshTokenReused
)

type RefreshTokenRotation struct {
	Status RefreshTokenRotationStatus
	UserID string
}

type RefreshTokenValidationStatus int

const (
	RefreshTokenValidationInvalid RefreshTokenValidationStatus = iota
	RefreshTokenValidationActive
	RefreshTokenValidationReused
)

type RefreshTokenValidation struct {
	Status RefreshTokenValidationStatus
	UserID string
}

type RefreshTokenStore interface {
	Issue(ctx context.Context, userID, token string, ttl time.Duration) error
	Validate(ctx context.Context, token string) (RefreshTokenValidation, error)
	Rotate(ctx context.Context, oldToken, newToken string, ttl time.Duration) (RefreshTokenRotation, error)
	Revoke(ctx context.Context, token string) error
	RevokeUser(ctx context.Context, userID string) error
}

var errRefreshTokenStoreNotConfigured = errors.New("refresh token store not configured")

type missingRefreshTokenStore struct{}

func (missingRefreshTokenStore) Issue(context.Context, string, string, time.Duration) error {
	return errRefreshTokenStoreNotConfigured
}

func (missingRefreshTokenStore) Validate(context.Context, string) (RefreshTokenValidation, error) {
	return RefreshTokenValidation{}, errRefreshTokenStoreNotConfigured
}

func (missingRefreshTokenStore) Rotate(context.Context, string, string, time.Duration) (RefreshTokenRotation, error) {
	return RefreshTokenRotation{}, errRefreshTokenStoreNotConfigured
}

func (missingRefreshTokenStore) Revoke(context.Context, string) error {
	return errRefreshTokenStoreNotConfigured
}

func (missingRefreshTokenStore) RevokeUser(context.Context, string) error {
	return errRefreshTokenStoreNotConfigured
}

type RedisRefreshTokenStore struct {
	client *redis.Client
}

func NewRedisRefreshTokenStore(client *redis.Client) *RedisRefreshTokenStore {
	return &RedisRefreshTokenStore{client: client}
}

const (
	refreshTokenActivePrefix = "auth:rt:active:"
	refreshTokenUsedPrefix   = "auth:rt:used:"
	refreshTokenUserPrefix   = "auth:rt:user:"
)

var issueRefreshTokenScript = redis.NewScript(`
local active_key = KEYS[1]
local user_index_key = KEYS[2]

local digest = ARGV[1]
local user_id = ARGV[2]
local ttl_ms = tonumber(ARGV[3])
local expires_at_ms = tonumber(ARGV[4])

local created = redis.call("SET", active_key, user_id, "NX", "PX", ttl_ms)
if not created then
	return 0
end

redis.call("ZREMRANGEBYSCORE", user_index_key, "-inf", expires_at_ms - ttl_ms)
redis.call("ZADD", user_index_key, expires_at_ms, digest)
redis.call("PEXPIRE", user_index_key, ttl_ms)
return 1
`)

var validateRefreshTokenScript = redis.NewScript(`
local active_key = KEYS[1]
local used_key = KEYS[2]

local user_index_prefix = ARGV[1]
local active_prefix = ARGV[2]

local user_id = redis.call("GET", active_key)
if user_id then
	return {1, user_id}
end

user_id = redis.call("GET", used_key)
if user_id then
	local user_index_key = user_index_prefix .. user_id
	local active_digests = redis.call("ZRANGE", user_index_key, 0, -1)
	for _, digest in ipairs(active_digests) do
		redis.call("DEL", active_prefix .. digest)
	end
	redis.call("DEL", user_index_key)
	return {2, user_id}
end

return {0, ""}
`)

var rotateRefreshTokenScript = redis.NewScript(`
local old_active_key = KEYS[1]
local old_used_key = KEYS[2]
local new_active_key = KEYS[3]

local old_digest = ARGV[1]
local new_digest = ARGV[2]
local ttl_ms = tonumber(ARGV[3])
local now_ms = tonumber(ARGV[4])
local user_index_prefix = ARGV[5]
local active_prefix = ARGV[6]

local user_id = redis.call("GET", old_active_key)
if user_id then
	local remaining_ttl_ms = redis.call("PTTL", old_active_key)
	if remaining_ttl_ms < 1 then
		remaining_ttl_ms = ttl_ms
	end

	redis.call("DEL", old_active_key)
	redis.call("SET", old_used_key, user_id, "PX", remaining_ttl_ms)
	redis.call("SET", new_active_key, user_id, "PX", ttl_ms)

	local user_index_key = user_index_prefix .. user_id
	redis.call("ZREMRANGEBYSCORE", user_index_key, "-inf", now_ms)
	redis.call("ZREM", user_index_key, old_digest)
	redis.call("ZADD", user_index_key, now_ms + ttl_ms, new_digest)
	redis.call("PEXPIRE", user_index_key, ttl_ms)
	return {1, user_id}
end

user_id = redis.call("GET", old_used_key)
if user_id then
	local user_index_key = user_index_prefix .. user_id
	local active_digests = redis.call("ZRANGE", user_index_key, 0, -1)
	for _, digest in ipairs(active_digests) do
		redis.call("DEL", active_prefix .. digest)
	end
	redis.call("DEL", user_index_key)
	return {2, user_id}
end

return {0, ""}
`)

var revokeRefreshTokenScript = redis.NewScript(`
local active_key = KEYS[1]

local digest = ARGV[1]
local user_index_prefix = ARGV[2]

local user_id = redis.call("GET", active_key)
if user_id then
	redis.call("DEL", active_key)
	redis.call("ZREM", user_index_prefix .. user_id, digest)
end
return 1
`)

var revokeRefreshTokensByUserScript = redis.NewScript(`
local user_index_key = KEYS[1]
local active_prefix = ARGV[1]

local active_digests = redis.call("ZRANGE", user_index_key, 0, -1)
for _, digest in ipairs(active_digests) do
	redis.call("DEL", active_prefix .. digest)
end
redis.call("DEL", user_index_key)
return #active_digests
`)

func (s *RedisRefreshTokenStore) Issue(ctx context.Context, userID, token string, ttl time.Duration) error {
	if err := s.validateClient(); err != nil {
		return err
	}
	if err := validateRefreshTokenStoreInput(userID, token, ttl); err != nil {
		return err
	}

	digest := auth.HashToken(token)
	now := time.Now()
	created, err := issueRefreshTokenScript.Run(ctx, s.client,
		[]string{s.activeKey(digest), s.userIndexKey(userID)},
		digest,
		userID,
		ttl.Milliseconds(),
		now.Add(ttl).UnixMilli(),
	).Int()
	if err != nil {
		return fmt.Errorf("issue refresh token: %w", err)
	}
	if created != 1 {
		return errors.New("issue refresh token: digest already active")
	}
	return nil
}

func (s *RedisRefreshTokenStore) Validate(ctx context.Context, token string) (RefreshTokenValidation, error) {
	if err := s.validateClient(); err != nil {
		return RefreshTokenValidation{}, err
	}
	if token == "" {
		return RefreshTokenValidation{}, errors.New("refresh token is required")
	}

	digest := auth.HashToken(token)
	result, err := validateRefreshTokenScript.Run(ctx, s.client,
		[]string{s.activeKey(digest), s.usedKey(digest)},
		refreshTokenUserPrefix,
		refreshTokenActivePrefix,
	).Result()
	if err != nil {
		return RefreshTokenValidation{}, fmt.Errorf("validate refresh token: %w", err)
	}

	return parseRefreshTokenValidation(result)
}

func (s *RedisRefreshTokenStore) Rotate(ctx context.Context, oldToken, newToken string, ttl time.Duration) (RefreshTokenRotation, error) {
	if err := s.validateClient(); err != nil {
		return RefreshTokenRotation{}, err
	}
	if oldToken == "" {
		return RefreshTokenRotation{}, errors.New("refresh token is required")
	}
	if newToken == "" {
		return RefreshTokenRotation{}, errors.New("refresh token is required")
	}
	if ttl <= 0 {
		return RefreshTokenRotation{}, errors.New("refresh token ttl must be positive")
	}

	oldDigest := auth.HashToken(oldToken)
	newDigest := auth.HashToken(newToken)
	result, err := rotateRefreshTokenScript.Run(ctx, s.client,
		[]string{s.activeKey(oldDigest), s.usedKey(oldDigest), s.activeKey(newDigest)},
		oldDigest,
		newDigest,
		ttl.Milliseconds(),
		time.Now().UnixMilli(),
		refreshTokenUserPrefix,
		refreshTokenActivePrefix,
	).Result()
	if err != nil {
		return RefreshTokenRotation{}, fmt.Errorf("rotate refresh token: %w", err)
	}

	return parseRefreshTokenRotation(result)
}

func (s *RedisRefreshTokenStore) Revoke(ctx context.Context, token string) error {
	if err := s.validateClient(); err != nil {
		return err
	}
	if token == "" {
		return errors.New("refresh token is required")
	}

	digest := auth.HashToken(token)
	if err := revokeRefreshTokenScript.Run(ctx, s.client,
		[]string{s.activeKey(digest)},
		digest,
		refreshTokenUserPrefix,
	).Err(); err != nil {
		return fmt.Errorf("revoke refresh token: %w", err)
	}
	return nil
}

func (s *RedisRefreshTokenStore) RevokeUser(ctx context.Context, userID string) error {
	if err := s.validateClient(); err != nil {
		return err
	}
	if userID == "" {
		return errors.New("user id is required")
	}

	if err := revokeRefreshTokensByUserScript.Run(ctx, s.client,
		[]string{s.userIndexKey(userID)},
		refreshTokenActivePrefix,
	).Err(); err != nil {
		return fmt.Errorf("revoke user refresh tokens: %w", err)
	}
	return nil
}

func (s *RedisRefreshTokenStore) activeKey(digest string) string {
	return refreshTokenActivePrefix + digest
}

func (s *RedisRefreshTokenStore) usedKey(digest string) string {
	return refreshTokenUsedPrefix + digest
}

func (s *RedisRefreshTokenStore) userIndexKey(userID string) string {
	return refreshTokenUserPrefix + userID
}

func (s *RedisRefreshTokenStore) validateClient() error {
	if s == nil || s.client == nil {
		return errors.New("redis refresh token store is not configured")
	}
	return nil
}

func validateRefreshTokenStoreInput(userID, token string, ttl time.Duration) error {
	if userID == "" {
		return errors.New("user id is required")
	}
	if token == "" {
		return errors.New("refresh token is required")
	}
	if ttl <= 0 {
		return errors.New("refresh token ttl must be positive")
	}
	return nil
}

func parseRefreshTokenValidation(result any) (RefreshTokenValidation, error) {
	values, ok := result.([]interface{})
	if !ok || len(values) != 2 {
		return RefreshTokenValidation{}, fmt.Errorf("unexpected refresh token validation result: %T", result)
	}

	statusCode, err := parseLuaInt(values[0])
	if err != nil {
		return RefreshTokenValidation{}, err
	}
	userID, ok := values[1].(string)
	if !ok {
		return RefreshTokenValidation{}, fmt.Errorf("unexpected refresh token validation user id type: %T", values[1])
	}

	switch statusCode {
	case 0:
		return RefreshTokenValidation{Status: RefreshTokenValidationInvalid}, nil
	case 1:
		return RefreshTokenValidation{Status: RefreshTokenValidationActive, UserID: userID}, nil
	case 2:
		return RefreshTokenValidation{Status: RefreshTokenValidationReused, UserID: userID}, nil
	default:
		return RefreshTokenValidation{}, fmt.Errorf("unexpected refresh token validation status: %d", statusCode)
	}
}

func parseRefreshTokenRotation(result any) (RefreshTokenRotation, error) {
	values, ok := result.([]interface{})
	if !ok || len(values) != 2 {
		return RefreshTokenRotation{}, fmt.Errorf("unexpected refresh token rotation result: %T", result)
	}

	statusCode, err := parseLuaInt(values[0])
	if err != nil {
		return RefreshTokenRotation{}, err
	}
	userID, ok := values[1].(string)
	if !ok {
		return RefreshTokenRotation{}, fmt.Errorf("unexpected refresh token rotation user id type: %T", values[1])
	}

	switch statusCode {
	case 0:
		return RefreshTokenRotation{Status: RefreshTokenInvalid}, nil
	case 1:
		return RefreshTokenRotation{Status: RefreshTokenRotated, UserID: userID}, nil
	case 2:
		return RefreshTokenRotation{Status: RefreshTokenReused, UserID: userID}, nil
	default:
		return RefreshTokenRotation{}, fmt.Errorf("unexpected refresh token rotation status: %d", statusCode)
	}
}

func parseLuaInt(value any) (int64, error) {
	switch v := value.(type) {
	case int64:
		return v, nil
	case string:
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("unexpected lua integer value %q: %w", v, err)
		}
		return n, nil
	default:
		return 0, fmt.Errorf("unexpected lua integer type: %T", value)
	}
}
