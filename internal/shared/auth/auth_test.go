package auth

import (
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenerateJWT(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		userID    string
		username  string
		secretKey string
		duration  time.Duration
		wantErr   bool
	}{
		{
			name:      "Success: 유효한 파라미터로 JWT 생성",
			userID:    "user123",
			username:  "testuser",
			secretKey: "test_secret",
			duration:  15 * time.Minute,
			wantErr:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := GenerateJWT(tt.userID, tt.username, tt.secretKey, tt.duration)
			if tt.wantErr {
				require.Error(t, err)
				assert.Empty(t, got)
			} else {
				require.NoError(t, err)
				assert.NotEmpty(t, got)
			}
		})
	}
}

func TestVerifyJWT(t *testing.T) {
	t.Parallel()
	secret := "test_secret"
	wrongSecret := "wrong_secret"

	validClaims := func() UserClaims {
		return UserClaims{
			Username: "testuser",
			RegisteredClaims: jwt.RegisteredClaims{
				Subject:   "user123",
				ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
				IssuedAt:  jwt.NewNumericDate(time.Now()),
			},
		}
	}

	tests := []struct {
		name        string
		tokenString func() string
		wantErr     bool
		errMsg      string
	}{
		{
			name: "Success: 유효한 토큰 검증 성공",
			tokenString: func() string {
				token := jwt.NewWithClaims(jwt.SigningMethodHS256, validClaims())
				s, _ := token.SignedString([]byte(secret))
				return s
			},
			wantErr: false,
		},
		{
			name: "Failure: 만료된 토큰 검증 실패",
			tokenString: func() string {
				claims := validClaims()
				claims.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-1 * time.Hour))
				token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
				s, _ := token.SignedString([]byte(secret))
				return s
			},
			wantErr: true,
			errMsg:  "token is expired",
		},
		{
			name: "Failure: 서명이 잘못된 토큰 검증 실패",
			tokenString: func() string {
				token := jwt.NewWithClaims(jwt.SigningMethodHS256, validClaims())
				s, _ := token.SignedString([]byte(wrongSecret))
				return s
			},
			wantErr: true,
			errMsg:  "signature is invalid",
		},
		{
			name: "Failure: 지원하지 않는 알고리즘(None) 사용 시 실패",
			tokenString: func() string {
				token := jwt.NewWithClaims(jwt.SigningMethodNone, validClaims())
				s, _ := token.SignedString(jwt.UnsafeAllowNoneSignatureType)
				return s
			},
			wantErr: true,
			errMsg:  "signing method none is invalid",
		},
		{
			name: "Failure: 형식이 잘못된 토큰 문자열",
			tokenString: func() string {
				return "invalid.token.string"
			},
			wantErr: true,
			errMsg:  "token is malformed",
		},
		{
			name: "Failure: 위변조된 서명 검증 실패",
			tokenString: func() string {
				token := jwt.NewWithClaims(jwt.SigningMethodHS256, validClaims())
				s, _ := token.SignedString([]byte(secret))
				parts := strings.Split(s, ".")
				parts[2] = parts[2] + "tampered"
				return strings.Join(parts, ".")
			},
			wantErr: true,
			errMsg:  "signature is invalid",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := VerifyJWT(tt.tokenString(), secret)

			if tt.wantErr {
				require.Error(t, err)
				if tt.errMsg != "" {
					assert.Contains(t, err.Error(), tt.errMsg)
				}
				assert.Nil(t, got)
			} else {
				require.NoError(t, err)
				assert.NotNil(t, got)
				assert.Equal(t, "testuser", got.Username)
			}
		})
	}
}

func TestHashToken(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		token string
	}{
		{"Success: 빈 문자열 해싱", ""},
		{"Success: 짧은 문자열 해싱", "abc"},
		{"Success: 긴 문자열 해싱", "this-is-a-very-long-token-string-for-testing"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := HashToken(tt.token)
			require.NotEmpty(t, got)
			assert.Equal(t, got, HashToken(tt.token), "same token should return same hash")
			assert.NotEqual(t, got, HashToken(tt.token+"x"), "different tokens should return different hashes")
		})
	}
}
