package user

import (
	"context"
	pb "go-chat-msa/api/proto/user/v1"
	"go-chat-msa/internal/shared/config"
	"go-chat-msa/internal/user/db"
	"go-chat-msa/internal/user/hasher"
)

type Service struct {
	pb.UnsafeUserServiceServer
	config    config.UserConfig
	secretKey string
	queries   db.Querier
	hasher    *hasher.Pool
	tokens    RefreshTokenStore
	runInTx   func(ctx context.Context, fn func(db.Querier) error) error
}

func NewService(dbConn db.Querier, cfg config.UserConfig, secretKey string, h *hasher.Pool) *Service {
	return &Service{
		queries:   dbConn,
		config:    cfg,
		secretKey: secretKey,
		hasher:    h,
		tokens:    missingRefreshTokenStore{},

		runInTx: func(_ context.Context, fn func(db.Querier) error) error {
			return fn(dbConn)
		},
	}
}

func (s *Service) WithRunInTx(runInTx func(ctx context.Context, fn func(db.Querier) error) error) *Service {
	s.runInTx = runInTx
	return s
}

func (s *Service) WithRefreshTokenStore(tokens RefreshTokenStore) *Service {
	if tokens == nil {
		s.tokens = missingRefreshTokenStore{}
		return s
	}
	s.tokens = tokens
	return s
}
