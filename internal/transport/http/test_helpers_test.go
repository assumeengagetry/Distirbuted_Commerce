package httptransport

import (
	"context"
	"io"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/assumeengagetry/distributed-commerce/internal/auth"
	"github.com/assumeengagetry/distributed-commerce/internal/user"
)

var transportTestNow = time.Date(2026, time.August, 10, 12, 0, 0, 0, time.UTC)

type stubUserService struct {
	register   func(context.Context, user.RegisterRequest) (user.AuthResult, error)
	login      func(context.Context, user.LoginRequest) (user.AuthResult, error)
	refresh    func(context.Context, string) (user.AuthResult, error)
	logout     func(context.Context, string) error
	getProfile func(context.Context, uuid.UUID) (user.Profile, error)
}

func (s *stubUserService) Register(ctx context.Context, req user.RegisterRequest) (user.AuthResult, error) {
	if s.register == nil {
		return user.AuthResult{}, nil
	}
	return s.register(ctx, req)
}

func (s *stubUserService) Login(ctx context.Context, req user.LoginRequest) (user.AuthResult, error) {
	if s.login == nil {
		return user.AuthResult{}, nil
	}
	return s.login(ctx, req)
}

func (s *stubUserService) Refresh(ctx context.Context, token string) (user.AuthResult, error) {
	if s.refresh == nil {
		return user.AuthResult{}, nil
	}
	return s.refresh(ctx, token)
}

func (s *stubUserService) Logout(ctx context.Context, token string) error {
	if s.logout == nil {
		return nil
	}
	return s.logout(ctx, token)
}

func (s *stubUserService) GetProfile(ctx context.Context, userID uuid.UUID) (user.Profile, error) {
	if s.getProfile == nil {
		return user.Profile{}, nil
	}
	return s.getProfile(ctx, userID)
}

type stubTokenVerifier struct {
	principal auth.Principal
	err       error
}

func (s stubTokenVerifier) ParseAccess(string, time.Time) (auth.Principal, error) {
	return s.principal, s.err
}

func baseTestDependencies(check func(context.Context) error, timeout time.Duration) Dependencies {
	return Dependencies{
		Logger:           slog.New(slog.NewJSONHandler(io.Discard, nil)),
		ServiceName:      "user-service",
		ReadinessCheck:   check,
		ReadinessTimeout: timeout,
		UserService:      &stubUserService{},
		TokenVerifier: stubTokenVerifier{principal: auth.Principal{
			UserID: uuid.New(), Role: string(user.RoleCustomer), TokenID: uuid.New(),
		}},
		AuthRateLimit: RateLimitConfig{
			RequestsPerSecond: 1000,
			Burst:             100,
			EntryTTL:          time.Minute,
			MaxEntries:        100,
		},
		Now: func() time.Time { return transportTestNow },
	}
}
