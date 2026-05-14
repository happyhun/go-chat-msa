package user

import (
	"context"
	"sync"
	"testing"
	"time"

	"go-chat-msa/internal/shared/auth"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestRefreshTokenStore(t *testing.T) (*RedisRefreshTokenStore, *miniredis.Miniredis) {
	t.Helper()

	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() {
		require.NoError(t, client.Close())
	})

	return NewRedisRefreshTokenStore(client), mr
}

func TestRedisRefreshTokenStore_IssueAndRotate(t *testing.T) {
	t.Parallel()

	store, mr := newTestRefreshTokenStore(t)
	ctx := t.Context()
	userID := "user-1"
	oldToken := "old-refresh-token"
	newToken := "new-refresh-token"
	ttl := time.Hour

	require.NoError(t, store.Issue(ctx, userID, oldToken, ttl))

	oldDigest := auth.HashToken(oldToken)
	assert.Equal(t, userID, mustGetRedisString(t, mr, refreshTokenActivePrefix+oldDigest))
	assert.True(t, mr.Exists(refreshTokenUserPrefix+userID))

	rotation, err := store.Rotate(ctx, oldToken, newToken, ttl)
	require.NoError(t, err)
	assert.Equal(t, RefreshTokenRotated, rotation.Status)
	assert.Equal(t, userID, rotation.UserID)

	newDigest := auth.HashToken(newToken)
	assert.False(t, mr.Exists(refreshTokenActivePrefix+oldDigest))
	assert.Equal(t, userID, mustGetRedisString(t, mr, refreshTokenUsedPrefix+oldDigest))
	assert.Equal(t, userID, mustGetRedisString(t, mr, refreshTokenActivePrefix+newDigest))
}

func TestRedisRefreshTokenStore_RotateInvalid(t *testing.T) {
	t.Parallel()

	store, _ := newTestRefreshTokenStore(t)

	rotation, err := store.Rotate(t.Context(), "missing-token", "new-token", time.Hour)
	require.NoError(t, err)
	assert.Equal(t, RefreshTokenInvalid, rotation.Status)
	assert.Empty(t, rotation.UserID)
}

func TestRedisRefreshTokenStore_ReuseRevokesUserTokens(t *testing.T) {
	t.Parallel()

	store, mr := newTestRefreshTokenStore(t)
	ctx := t.Context()
	userID := "user-1"
	oldToken := "old-refresh-token"
	newToken := "new-refresh-token"

	require.NoError(t, store.Issue(ctx, userID, oldToken, time.Hour))
	rotation, err := store.Rotate(ctx, oldToken, newToken, time.Hour)
	require.NoError(t, err)
	require.Equal(t, RefreshTokenRotated, rotation.Status)

	reuse, err := store.Rotate(ctx, oldToken, "attacker-new-token", time.Hour)
	require.NoError(t, err)
	assert.Equal(t, RefreshTokenReused, reuse.Status)
	assert.Equal(t, userID, reuse.UserID)
	assert.False(t, mr.Exists(refreshTokenActivePrefix+auth.HashToken(newToken)))
	assert.False(t, mr.Exists(refreshTokenUserPrefix+userID))
}

func TestRedisRefreshTokenStore_ConcurrentRotateSingleWinner(t *testing.T) {
	t.Parallel()

	store, _ := newTestRefreshTokenStore(t)
	ctx := context.Background()
	userID := "user-1"
	oldToken := "old-refresh-token"
	require.NoError(t, store.Issue(ctx, userID, oldToken, time.Hour))

	const attempts = 8
	var wg sync.WaitGroup
	statuses := make([]RefreshTokenRotationStatus, attempts)
	errs := make([]error, attempts)
	for i := range attempts {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			rotation, err := store.Rotate(ctx, oldToken, "new-token-"+time.Now().String(), time.Hour)
			errs[idx] = err
			statuses[idx] = rotation.Status
		}(i)
	}
	wg.Wait()

	rotated := 0
	reused := 0
	for i, err := range errs {
		require.NoError(t, err)
		switch statuses[i] {
		case RefreshTokenRotated:
			rotated++
		case RefreshTokenReused:
			reused++
		default:
			t.Fatalf("unexpected status %v", statuses[i])
		}
	}
	assert.Equal(t, 1, rotated)
	assert.Equal(t, attempts-1, reused)
}

func TestRedisRefreshTokenStore_Revoke(t *testing.T) {
	t.Parallel()

	store, mr := newTestRefreshTokenStore(t)
	ctx := t.Context()
	userID := "user-1"
	token := "refresh-token"
	require.NoError(t, store.Issue(ctx, userID, token, time.Hour))

	require.NoError(t, store.Revoke(ctx, token))

	assert.False(t, mr.Exists(refreshTokenActivePrefix+auth.HashToken(token)))
}

func TestRedisRefreshTokenStore_RevokeUser(t *testing.T) {
	t.Parallel()

	store, mr := newTestRefreshTokenStore(t)
	ctx := t.Context()
	userID := "user-1"
	token1 := "refresh-token-1"
	token2 := "refresh-token-2"
	require.NoError(t, store.Issue(ctx, userID, token1, time.Hour))
	require.NoError(t, store.Issue(ctx, userID, token2, time.Hour))

	require.NoError(t, store.RevokeUser(ctx, userID))

	assert.False(t, mr.Exists(refreshTokenActivePrefix+auth.HashToken(token1)))
	assert.False(t, mr.Exists(refreshTokenActivePrefix+auth.HashToken(token2)))
	assert.False(t, mr.Exists(refreshTokenUserPrefix+userID))
}

func TestRedisRefreshTokenStore_ExpiredTokenIsInvalid(t *testing.T) {
	t.Parallel()

	store, mr := newTestRefreshTokenStore(t)
	ctx := t.Context()
	require.NoError(t, store.Issue(ctx, "user-1", "refresh-token", time.Second))

	mr.FastForward(time.Second + time.Millisecond)

	rotation, err := store.Rotate(ctx, "refresh-token", "new-token", time.Hour)
	require.NoError(t, err)
	assert.Equal(t, RefreshTokenInvalid, rotation.Status)
}

func mustGetRedisString(t *testing.T, mr *miniredis.Miniredis, key string) string {
	t.Helper()

	value, err := mr.Get(key)
	require.NoError(t, err)
	return value
}
