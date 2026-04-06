package loadbalance

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestHashRing_New(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		endpoints []string
		key       string
		expected  assert.ValueAssertionFunc
	}{
		{
			name:      "Success: 빈 엔드포인트 목록 처리",
			endpoints: []string{},
			key:       "any",
			expected: func(t assert.TestingT, v any, msgAndArgs ...any) bool {
				return assert.Empty(t, v, msgAndArgs...)
			},
		},
		{
			name:      "Success: 엔드포인트가 있을 때 정상 할당",
			endpoints: []string{"ws-1", "ws-2"},
			key:       "room1",
			expected: func(t assert.TestingT, v any, msgAndArgs ...any) bool {
				endpoints := []string{"ws-1", "ws-2"}
				return assert.Contains(t, endpoints, v, msgAndArgs...)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ring := New(tt.endpoints)
			assert.NotNil(t, ring)
			tt.expected(t, ring.Locate(tt.key))
		})
	}
}

func TestHashRing_Locate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{
			name: "Success: 일관된 해싱 (Consistency)",
			run: func(t *testing.T) {
				endpoints := []string{"node1", "node2", "node3"}
				ring := New(endpoints)
				roomID := "room-123"
				expected := ring.Locate(roomID)

				for range 100 {
					assert.Equal(t, expected, ring.Locate(roomID))
				}
			},
		},
		{
			name: "Success: 균등한 분포 (Distribution)",
			run: func(t *testing.T) {
				endpoints := []string{"srvA", "srvB", "srvC"}
				ring := New(endpoints)
				distribution := make(map[string]int)

				for i := range 1000 {
					roomID := "room-" + strconv.Itoa(i)
					node := ring.Locate(roomID)
					distribution[node]++
				}

				assert.Len(t, distribution, 3)
				for _, count := range distribution {
					assert.Greater(t, count, 100)
				}
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

func TestHasher_Sum64(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{
			name: "Success: 동일 데이터에 대해 일관된 해시값 생성",
			run: func(t *testing.T) {
				h := hasher{}
				data := []byte("test-data")

				hash1 := h.Sum64(data)
				hash2 := h.Sum64(data)
				assert.Equal(t, hash1, hash2)
				assert.NotZero(t, hash1)
			},
		},
		{
			name: "Success: 다른 데이터에 대해 다른 해시값 생성",
			run: func(t *testing.T) {
				h := hasher{}
				hash1 := h.Sum64([]byte("data1"))
				hash2 := h.Sum64([]byte("different"))
				assert.NotEqual(t, hash1, hash2)
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
