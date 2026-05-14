package user

import (
	"context"
	"log/slog"
)

type tokenPurgeStore interface {
	DeleteExpiredRefreshTokens(context.Context) error
}

func PurgeExpiredTokensOnce(ctx context.Context, store tokenPurgeStore) error {
	if err := store.DeleteExpiredRefreshTokens(ctx); err != nil {
		slog.ErrorContext(ctx, "failed to purge expired tokens", "error", err)
		return err
	}
	slog.InfoContext(ctx, "purged expired refresh tokens")
	return nil
}

func (s *Service) PurgeExpiredTokensOnce(ctx context.Context) error {
	return PurgeExpiredTokensOnce(ctx, s.queries)
}
