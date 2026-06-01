package membership

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDedupeMemberKeys(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		keys   []string
		prefix string
		want   []string
	}{
		{
			name:   "Success: 빈 입력",
			keys:   nil,
			prefix: "wss:member:",
			want:   []string{},
		},
		{
			name:   "Success: 중복 없는 입력",
			keys:   []string{"wss:member:a:8081", "wss:member:b:8081"},
			prefix: "wss:member:",
			want:   []string{"a:8081", "b:8081"},
		},
		{
			name:   "Success: 입력 순서와 무관하게 정렬",
			keys:   []string{"wss:member:b:8081", "wss:member:a:8081", "wss:member:b:8081"},
			prefix: "wss:member:",
			want:   []string{"a:8081", "b:8081"},
		},
		{
			name:   "Success: SCAN이 같은 키를 두 번 반환한 경우 dedupe",
			keys:   []string{"wss:member:a:8081", "wss:member:b:8081", "wss:member:a:8081"},
			prefix: "wss:member:",
			want:   []string{"a:8081", "b:8081"},
		},
		{
			name:   "Success: 모든 항목이 동일한 극단 케이스",
			keys:   []string{"wss:member:x:8081", "wss:member:x:8081", "wss:member:x:8081"},
			prefix: "wss:member:",
			want:   []string{"x:8081"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := dedupeMemberKeys(tt.keys, tt.prefix)
			assert.Equal(t, tt.want, got)
		})
	}
}
