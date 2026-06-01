package loadbalance

import (
	"strconv"
	"sync"
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
			name: "Success: 멤버 입력 순서와 무관하게 같은 owner 반환",
			run: func(t *testing.T) {
				ring1 := New([]string{"node1", "node2", "node3"})
				ring2 := New([]string{"node3", "node1", "node2"})

				for i := range 200 {
					roomID := "room-" + strconv.Itoa(i)
					assert.Equal(t, ring1.Locate(roomID), ring2.Locate(roomID))
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

func ringMembers(ring *HashRing) []string {
	ms := ring.hash.GetMembers()
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.String()
	}
	return out
}

func TestHashRing_Set(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{
			name: "Success: 빈 ring을 Set으로 채움",
			run: func(t *testing.T) {
				ring := New(nil)
				ring.Set([]string{"ws-1", "ws-2"})
				assert.ElementsMatch(t, []string{"ws-1", "ws-2"}, ringMembers(ring))
			},
		},
		{
			name: "Success: Set으로 멤버 교체 (diff 적용)",
			run: func(t *testing.T) {
				ring := New([]string{"ws-1", "ws-2"})
				ring.Set([]string{"ws-2", "ws-3"})
				assert.ElementsMatch(t, []string{"ws-2", "ws-3"}, ringMembers(ring))
			},
		},
		{
			name: "Success: 동일 멤버를 Set하면 변화 없음",
			run: func(t *testing.T) {
				ring := New([]string{"ws-1", "ws-2"})
				ring.Set([]string{"ws-1", "ws-2"})
				assert.ElementsMatch(t, []string{"ws-1", "ws-2"}, ringMembers(ring))
			},
		},
		{
			name: "Success: 빈 슬라이스 Set으로 모든 멤버 제거",
			run: func(t *testing.T) {
				ring := New([]string{"ws-1", "ws-2"})
				ring.Set([]string{})
				assert.Empty(t, ringMembers(ring))
			},
		},
		{
			name: "Success: 모든 멤버 제거 후 Locate는 빈 문자열",
			run: func(t *testing.T) {
				ring := New([]string{"ws-1"})
				ring.Set(nil)
				assert.Empty(t, ring.Locate("room"))
			},
		},
		{
			name: "Success: Set 후에도 Locate는 일관된 결과",
			run: func(t *testing.T) {
				ring := New([]string{"ws-1", "ws-2", "ws-3"})
				before := ring.Locate("room-x")
				ring.Set([]string{"ws-1", "ws-2", "ws-3"})
				after := ring.Locate("room-x")
				assert.Equal(t, before, after)
			},
		},
		{
			name: "Success: Set 입력 순서와 무관하게 같은 owner 반환",
			run: func(t *testing.T) {
				ring1 := New([]string{"ws-1", "ws-2"})
				ring2 := New([]string{"ws-2", "ws-1"})

				ring1.Set([]string{"ws-3", "ws-1", "ws-2"})
				ring2.Set([]string{"ws-2", "ws-3", "ws-1"})

				for i := range 200 {
					roomID := "room-" + strconv.Itoa(i)
					assert.Equal(t, ring1.Locate(roomID), ring2.Locate(roomID))
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

func TestHashRing_Len(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		addrs    []string
		expected int
	}{
		{name: "Success: 빈 ring", addrs: nil, expected: 0},
		{name: "Success: 멤버 3개", addrs: []string{"ws-1", "ws-2", "ws-3"}, expected: 3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ring := New(tt.addrs)
			assert.Equal(t, tt.expected, ring.Len())
		})
	}
}

func TestHashRing_SetLocateConcurrent(t *testing.T) {
	t.Parallel()

	ring := New([]string{"ws-1", "ws-2"})

	rounds := 200
	var wg sync.WaitGroup

	wg.Go(func() {
		for i := range rounds {
			if i%2 == 0 {
				ring.Set([]string{"ws-1", "ws-2", "ws-3"})
			} else {
				ring.Set([]string{"ws-1", "ws-2"})
			}
		}
	})

	wg.Go(func() {
		for i := range rounds {
			roomID := "room-" + strconv.Itoa(i)
			got := ring.Locate(roomID)
			assert.NotEmpty(t, got, "Locate는 항상 멤버를 반환해야 함")
		}
	})

	wg.Wait()
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
