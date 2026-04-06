package hasher

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
)

func TestWorkerPool_HashPassword(t *testing.T) {
	t.Parallel()

	cfg := PoolConfig{
		Workers: 2,
		Buffer:  10,
	}

	tests := []struct {
		name     string
		password string
	}{
		{
			name:     "Success: 유효한 비밀번호의 해싱 성공",
			password: "mySecretPassword",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			wp := NewPool(cfg)
			defer wp.Close()

			hash, err := wp.HashPassword(t.Context(), tt.password)
			require.NoError(t, err)
			assert.NotEmpty(t, hash)

			err = bcrypt.CompareHashAndPassword([]byte(hash), []byte(tt.password))
			assert.NoError(t, err)
		})
	}
}

func TestWorkerPool_ComparePassword(t *testing.T) {
	t.Parallel()

	cfg := PoolConfig{
		Workers: 2,
		Buffer:  10,
	}

	password := "mySecretPassword"
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	require.NoError(t, err)
	hashedPassword := string(hash)

	tests := []struct {
		name     string
		hashed   string
		password string
		wantErr  bool
	}{
		{
			name:     "Success: 비밀번호와 해시값이 일치하는 경우",
			hashed:   hashedPassword,
			password: password,
			wantErr:  false,
		},
		{
			name:     "Failure: 비밀번호 불일치 핸들링",
			hashed:   hashedPassword,
			password: "wrongPassword",
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			wp := NewPool(cfg)
			defer wp.Close()

			err := wp.ComparePassword(t.Context(), tt.hashed, tt.password)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestWorkerPool_ContextCancel(t *testing.T) {
	t.Parallel()

	cfg := DefaultPoolConfig()

	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{
			name: "Failure: 컨텍스트 취소 시 즉각적인 에러 반환 확인",
			run: func(t *testing.T) {
				wp := NewPool(cfg)
				defer wp.Close()

				ctx, cancel := context.WithCancel(t.Context())
				cancel()

				_, err := wp.HashPassword(ctx, "test")
				assert.Error(t, err)
				assert.Equal(t, context.Canceled, err)
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
