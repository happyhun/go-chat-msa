package user

import (
	"testing"

	"go-chat-msa/internal/shared/config"

	"github.com/stretchr/testify/require"
)

func Test_validateUsername(t *testing.T) {
	t.Parallel()
	cfg := config.UserConfig{
		Validation: config.ValidationConfig{
			MinUsernameLength: 2,
			MaxUsernameLength: 10,
		},
	}

	tests := []struct {
		name     string
		username string
		wantErr  bool
	}{
		{"Success: 영숫자 조합 유저네임 허용", "user123", false},
		{"Success: 한글 등 비영어권 문자 허용", "홍길동", false},
		{"Failure: 유저네임이 너무 짧은 경우", "a", true},
		{"Failure: 유저네임이 너무 긴 경우", "verylongusername", true},
		{"Failure: 특수문자가 포함된 유저네임 차단", "user!", true},
		{"Failure: 공백이 포함된 유저네임 차단", "user name", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := validateUsername(tt.username, cfg)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func Test_validatePassword(t *testing.T) {
	t.Parallel()
	cfg := config.UserConfig{
		Validation: config.ValidationConfig{
			MinPasswordLength: 8,
			MaxPasswordLength: 20,
		},
	}

	tests := []struct {
		name     string
		password string
		wantErr  bool
	}{
		{"Success: 4가지 문자 유형(대, 소, 숫자, 특수) 모두 포함", "Secure123!", false},
		{"Success: 3가지 문자 유형(대, 소, 숫자) 포함", "Secure123", false},
		{"Success: 3가지 문자 유형(대, 소, 특수) 포함", "SecurePass!", false},
		{"Success: 3가지 문자 유형(소, 숫자, 특수) 포함", "secure123!", false},
		{"Failure: 비밀번호가 최소 길이 미만인 경우", "Short1!", true},
		{"Failure: 문자 유형이 2가지 이하인 경우", "secure123", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := validatePassword(tt.password, cfg)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func Test_validateRoomName(t *testing.T) {
	t.Parallel()
	maxLength := 50

	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"Success: 유효한 채팅방 이름", "My Room", false},
		{"Success: 한글 채팅방 이름", "채팅방", false},
		{"Success: 최대 길이 경계값", string(make([]rune, 50)), false},
		{"Failure: 빈 문자열", "", true},
		{"Failure: 최대 길이 초과", string(make([]rune, 51)), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := validateRoomName(tt.input, maxLength)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func Test_validateCapacity(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		capacity    int32
		maxCapacity int32
		wantErr     bool
	}{
		{"Success: 유효한 정원 설정", 50, 100, false},
		{"Failure: 정원이 0인 경우", 0, 100, true},
		{"Failure: 정원이 음수인 경우", -5, 100, true},
		{"Failure: 최대 정원을 초과한 경우", 150, 100, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := validateCapacity(tt.capacity, tt.maxCapacity)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
