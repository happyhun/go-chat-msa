package user

import (
	"context"
	"errors"
	pb "go-chat-msa/api/proto/user/v1"
	"go-chat-msa/internal/shared/auth"
	"go-chat-msa/internal/user/hasher"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"golang.org/x/crypto/bcrypt"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (s *Service) VerifyUser(ctx context.Context, req *pb.VerifyUserRequest) (*pb.VerifyUserResponse, error) {
	user, err := s.queries.GetUserByUsername(ctx, req.Username)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, status.Error(codes.Unauthenticated, "invalid username or password")
		}
		slog.ErrorContext(ctx, "failed to get user", "error", err)
		return nil, status.Error(codes.Internal, "failed to get user")
	}

	if err := s.hasher.ComparePassword(ctx, user.PasswordHash, req.Password); err != nil {
		switch {
		case errors.Is(err, hasher.ErrQueueFull):
			return nil, status.Error(codes.ResourceExhausted, "system overloaded")
		case errors.Is(err, hasher.ErrClosed):
			return nil, status.Error(codes.Unavailable, "service shutting down")
		case errors.Is(err, bcrypt.ErrMismatchedHashAndPassword):
			authLoginTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "error")))
			return nil, status.Error(codes.Unauthenticated, "invalid username or password")
		default:
			slog.ErrorContext(ctx, "failed to compare password", "error", err)
			authLoginTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "error")))
			return nil, status.Error(codes.Internal, "failed to verify user")
		}
	}

	accessToken, refreshToken, err := s.issueTokenPair(ctx, user.ID, user.Username)
	if err != nil {
		return nil, err
	}

	authLoginTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "ok")))
	return &pb.VerifyUserResponse{
		UserId:       user.ID.String(),
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
	}, nil
}

func (s *Service) RefreshToken(ctx context.Context, req *pb.RefreshTokenRequest) (*pb.RefreshTokenResponse, error) {
	validation, err := s.tokens.Validate(ctx, req.RefreshToken)
	if err != nil {
		slog.ErrorContext(ctx, "failed to validate refresh token", "error", err)
		return nil, status.Error(codes.Internal, "failed to refresh token")
	}

	switch validation.Status {
	case RefreshTokenValidationInvalid:
		return nil, status.Error(codes.Unauthenticated, "invalid refresh token")
	case RefreshTokenValidationReused:
		authTokenReuseTotal.Add(ctx, 1)
		slog.WarnContext(ctx, "refresh token reuse detected, revoking all tokens", "user_id", validation.UserID)
		return nil, status.Error(codes.Unauthenticated, "refresh token reuse detected")
	case RefreshTokenValidationActive:
	default:
		slog.ErrorContext(ctx, "unexpected refresh token validation status", "status", validation.Status)
		return nil, status.Error(codes.Internal, "failed to refresh token")
	}

	userUUID, err := toPGUUID(validation.UserID)
	if err != nil {
		slog.ErrorContext(ctx, "invalid user id in refresh token store", "user_id", validation.UserID, "error", err)
		return nil, status.Error(codes.Internal, "failed to refresh token")
	}

	user, err := s.queries.GetUserByID(ctx, userUUID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			if revokeErr := s.tokens.RevokeUser(ctx, validation.UserID); revokeErr != nil {
				slog.WarnContext(ctx, "failed to revoke refresh tokens for missing user", "user_id", validation.UserID, "error", revokeErr)
			}
			return nil, status.Error(codes.Unauthenticated, "user no longer exists")
		}
		slog.ErrorContext(ctx, "failed to get user for refresh", "error", err)
		return nil, status.Error(codes.Internal, "failed to get user")
	}

	accessToken, err := s.issueAccessToken(user.ID, user.Username)
	if err != nil {
		slog.ErrorContext(ctx, "failed to generate access token", "error", err)
		return nil, status.Error(codes.Internal, "failed to generate access token")
	}

	refreshToken := uuid.NewString()
	rotation, err := s.tokens.Rotate(ctx, req.RefreshToken, refreshToken, s.refreshTokenTTL())
	if err != nil {
		slog.ErrorContext(ctx, "failed to rotate refresh token", "error", err)
		return nil, status.Error(codes.Internal, "failed to refresh token")
	}

	switch rotation.Status {
	case RefreshTokenInvalid:
		return nil, status.Error(codes.Unauthenticated, "invalid refresh token")
	case RefreshTokenReused:
		authTokenReuseTotal.Add(ctx, 1)
		slog.WarnContext(ctx, "refresh token reuse detected, revoking all tokens", "user_id", rotation.UserID)
		return nil, status.Error(codes.Unauthenticated, "refresh token reuse detected")
	case RefreshTokenRotated:
		if rotation.UserID != validation.UserID {
			slog.ErrorContext(ctx, "refresh token owner changed during rotation",
				"validated_user_id", validation.UserID,
				"rotated_user_id", rotation.UserID,
			)
			return nil, status.Error(codes.Internal, "failed to refresh token")
		}
	default:
		slog.ErrorContext(ctx, "unexpected refresh token rotation status", "status", rotation.Status)
		return nil, status.Error(codes.Internal, "failed to refresh token")
	}

	return &pb.RefreshTokenResponse{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
	}, nil
}

func (s *Service) RevokeToken(ctx context.Context, req *pb.RevokeTokenRequest) (*pb.RevokeTokenResponse, error) {
	if err := s.tokens.Revoke(ctx, req.RefreshToken); err != nil {
		slog.ErrorContext(ctx, "failed to revoke token", "error", err)
		return nil, status.Error(codes.Internal, "failed to revoke token")
	}

	return &pb.RevokeTokenResponse{}, nil
}

func (s *Service) issueAccessToken(userID pgtype.UUID, username string) (string, error) {
	accessTokenDuration := time.Duration(s.config.Token.AccessTokenExpirationMinutes) * time.Minute
	return auth.GenerateJWT(userID.String(), username, s.secretKey, accessTokenDuration)
}

func (s *Service) issueTokenPair(ctx context.Context, userID pgtype.UUID, username string) (string, string, error) {
	accessToken, err := s.issueAccessToken(userID, username)
	if err != nil {
		slog.ErrorContext(ctx, "failed to generate access token", "error", err)
		return "", "", status.Error(codes.Internal, "failed to generate access token")
	}
	refreshToken := uuid.NewString()

	if err := s.tokens.Issue(ctx, userID.String(), refreshToken, s.refreshTokenTTL()); err != nil {
		slog.ErrorContext(ctx, "failed to save refresh token", "error", err)
		return "", "", status.Error(codes.Internal, "failed to save refresh token")
	}

	return accessToken, refreshToken, nil
}

func (s *Service) refreshTokenTTL() time.Duration {
	return time.Duration(s.config.Token.RefreshTokenExpirationDays) * 24 * time.Hour
}
