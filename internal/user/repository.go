package user

import (
	"context"
	"time"

	"github.com/google/uuid"
)

type CreateUserParams struct {
	UserID           uuid.UUID
	AccountID        uuid.UUID
	SessionID        uuid.UUID
	RefreshTokenID   uuid.UUID
	Email            string
	PasswordHash     string
	DisplayName      string
	Role             Role
	Currency         string
	RefreshTokenHash []byte
	SessionExpiresAt time.Time
	CreatedAt        time.Time
}

type CreateSessionParams struct {
	SessionID        uuid.UUID
	UserID           uuid.UUID
	RefreshTokenID   uuid.UUID
	RefreshTokenHash []byte
	SessionExpiresAt time.Time
	CreatedAt        time.Time
}

type RotateRefreshTokenParams struct {
	CurrentTokenID     uuid.UUID
	CurrentTokenHash   []byte
	ReplacementTokenID uuid.UUID
	ReplacementHash    []byte
	ReuseGrace         time.Duration
}

type RotationResult struct {
	Profile          Profile
	SessionExpiresAt time.Time
	RotatedAt        time.Time
}

type RevokeSessionParams struct {
	RefreshTokenID   uuid.UUID
	RefreshTokenHash []byte
}

type Repository interface {
	CreateUser(ctx context.Context, params CreateUserParams) (Profile, error)
	GetCredentialsByEmail(ctx context.Context, email string) (Credentials, error)
	CreateSession(ctx context.Context, params CreateSessionParams) error
	RotateRefreshToken(ctx context.Context, params RotateRefreshTokenParams) (RotationResult, error)
	RevokeSession(ctx context.Context, params RevokeSessionParams) error
	GetProfile(ctx context.Context, userID uuid.UUID) (Profile, error)
	DeleteExpiredSessions(ctx context.Context, batchSize int32) (int64, error)
}
