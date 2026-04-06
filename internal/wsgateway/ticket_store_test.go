package wsgateway

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestTicketStore_Lifecycle(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{
			name: "Success: 티켓 저장 및 1회성 사용 검증",
			run: func(t *testing.T) {
				store := NewTicketStore()
				defer store.Stop()

				ticket := "test-ticket"
				userID := "user-123"

				store.Set(ticket, userID, 1*time.Minute)

				storedUserID, ok := store.GetAndDelete(ticket)
				assert.True(t, ok)
				assert.Equal(t, userID, storedUserID)

				_, ok = store.GetAndDelete(ticket)
				assert.False(t, ok)
			},
		},
		{
			name: "Failure: TTL 만료 후 자동 삭제",
			run: func(t *testing.T) {
				store := NewTicketStore()
				defer store.Stop()

				ticket := "expiring-ticket"
				userID := "user-456"

				store.Set(ticket, userID, 10*time.Millisecond)

				time.Sleep(50 * time.Millisecond)

				_, ok := store.GetAndDelete(ticket)
				assert.False(t, ok)
			},
		},
		{
			name: "Success: Stop 호출 시 모든 리소스 정리",
			run: func(t *testing.T) {
				store := NewTicketStore()

				store.Set("t1", "u1", 1*time.Hour)
				store.Set("t2", "u2", 1*time.Hour)

				assert.Len(t, store.tickets, 2)

				store.Stop()

				assert.Empty(t, store.tickets)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.run(t)
		})
	}
}
