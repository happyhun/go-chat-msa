package retention

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeStore struct {
	roomsCount int64
	usersCount int64
	roomsErr   error
	usersErr   error

	roomsThreshold pgtype.Timestamptz
	usersThreshold pgtype.Timestamptz
	roomsCalled    bool
	usersCalled    bool
}

func (s *fakeStore) PurgeDeletedRooms(_ context.Context, threshold pgtype.Timestamptz) (int64, error) {
	s.roomsCalled = true
	s.roomsThreshold = threshold
	return s.roomsCount, s.roomsErr
}

func (s *fakeStore) PurgeDeletedUsers(_ context.Context, threshold pgtype.Timestamptz) (int64, error) {
	s.usersCalled = true
	s.usersThreshold = threshold
	return s.usersCount, s.usersErr
}

func TestPurgeDeleted(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)

	t.Run("purges rooms and users with retention threshold", func(t *testing.T) {
		t.Parallel()

		store := &fakeStore{roomsCount: 2, usersCount: 3}

		result, err := PurgeDeleted(t.Context(), store, 30, now)

		require.NoError(t, err)
		assert.True(t, store.roomsCalled)
		assert.True(t, store.usersCalled)
		assert.Equal(t, now.AddDate(0, 0, -30), store.roomsThreshold.Time)
		assert.Equal(t, store.roomsThreshold, store.usersThreshold)
		assert.Equal(t, int64(2), result.Rooms.Count)
		assert.Equal(t, int64(3), result.Users.Count)
		assert.Equal(t, store.roomsThreshold, result.Threshold)
	})

	t.Run("attempts both purges and returns joined errors", func(t *testing.T) {
		t.Parallel()

		roomsErr := errors.New("rooms failed")
		usersErr := errors.New("users failed")
		store := &fakeStore{roomsErr: roomsErr, usersErr: usersErr}

		result, err := PurgeDeleted(t.Context(), store, 30, now)

		require.Error(t, err)
		assert.True(t, store.roomsCalled)
		assert.True(t, store.usersCalled)
		assert.ErrorIs(t, err, roomsErr)
		assert.ErrorIs(t, err, usersErr)
		assert.ErrorIs(t, result.Rooms.Err, roomsErr)
		assert.ErrorIs(t, result.Users.Err, usersErr)
	})

	t.Run("rejects invalid retention days before purge", func(t *testing.T) {
		t.Parallel()

		store := &fakeStore{}

		result, err := PurgeDeleted(t.Context(), store, 0, now)

		require.Error(t, err)
		assert.False(t, store.roomsCalled)
		assert.False(t, store.usersCalled)
		assert.False(t, result.Threshold.Valid)
	})
}
