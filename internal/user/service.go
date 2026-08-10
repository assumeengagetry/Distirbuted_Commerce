package user

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/assumeengagetry/distributed-commerce/internal/auth"
)

const (
	minimumPasswordLength = 12
	maximumPasswordLength = 128
	defaultCurrency       = "USD"
)

type PasswordManager interface {
	Hash(password string) (string, error)
	Verify(password, encodedHash string) (bool, error)
}

type ServiceConfig struct {
	RefreshTTL        time.Duration
	RefreshReuseGrace time.Duration
	DatabaseTimeout   time.Duration
	Now               func() time.Time
}

type Service struct {
	repository        Repository
	passwords         PasswordManager
	tokens            *auth.TokenManager
	logger            *slog.Logger
	refreshTTL        time.Duration
	refreshReuseGrace time.Duration
	databaseTimeout   time.Duration
	now               func() time.Time
	dummyHash         string
}

type RegisterRequest struct {
	Email       string
	Password    string
	DisplayName string
}

type LoginRequest struct {
	Email    string
	Password string
}

func NewService(
	repository Repository,
	passwords PasswordManager,
	tokens *auth.TokenManager,
	logger *slog.Logger,
	cfg ServiceConfig,
) (*Service, error) {
	if repository == nil {
		return nil, fmt.Errorf("user repository is required")
	}
	if passwords == nil {
		return nil, fmt.Errorf("password manager is required")
	}
	if tokens == nil {
		return nil, fmt.Errorf("token manager is required")
	}
	if logger == nil {
		return nil, fmt.Errorf("logger is required")
	}
	if cfg.RefreshTTL <= 0 {
		return nil, fmt.Errorf("refresh token TTL must be positive")
	}
	if cfg.RefreshReuseGrace < 0 || cfg.RefreshReuseGrace > time.Minute {
		return nil, fmt.Errorf("refresh token reuse grace must be between 0 and 1 minute")
	}
	if cfg.DatabaseTimeout <= 0 {
		return nil, fmt.Errorf("database operation timeout must be positive")
	}
	if cfg.Now == nil {
		return nil, fmt.Errorf("clock is required")
	}

	dummyHash, err := passwords.Hash("invalid-user-password-value")
	if err != nil {
		return nil, fmt.Errorf("create dummy password hash: %w", err)
	}

	return &Service{
		repository:        repository,
		passwords:         passwords,
		tokens:            tokens,
		logger:            logger,
		refreshTTL:        cfg.RefreshTTL,
		refreshReuseGrace: cfg.RefreshReuseGrace,
		databaseTimeout:   cfg.DatabaseTimeout,
		now:               cfg.Now,
		dummyHash:         dummyHash,
	}, nil
}

func (s *Service) Register(ctx context.Context, req RegisterRequest) (AuthResult, error) {
	email, err := normalizeEmail(req.Email)
	if err != nil {
		return AuthResult{}, err
	}
	displayName, err := normalizeDisplayName(req.DisplayName)
	if err != nil {
		return AuthResult{}, err
	}
	if err := validatePassword(req.Password); err != nil {
		return AuthResult{}, err
	}

	passwordHash, err := s.passwords.Hash(req.Password)
	if err != nil {
		if errors.Is(err, auth.ErrPasswordHasherBusy) {
			return AuthResult{}, ErrTemporarilyUnavailable
		}
		return AuthResult{}, fmt.Errorf("hash password: %w", err)
	}
	ids, err := newIdentityIDs()
	if err != nil {
		return AuthResult{}, err
	}
	refreshToken, err := auth.NewRefreshToken()
	if err != nil {
		return AuthResult{}, err
	}
	now := s.now().UTC()
	refreshExpiresAt := now.Add(s.refreshTTL)
	accessToken, accessExpiresAt, err := s.tokens.IssueAccess(ids.userID, string(RoleCustomer), now)
	if err != nil {
		return AuthResult{}, err
	}

	dbCtx, cancel := context.WithTimeout(ctx, s.databaseTimeout)
	profile, err := s.repository.CreateUser(dbCtx, CreateUserParams{
		UserID:           ids.userID,
		AccountID:        ids.accountID,
		SessionID:        ids.sessionID,
		RefreshTokenID:   refreshToken.ID,
		Email:            email,
		PasswordHash:     passwordHash,
		DisplayName:      displayName,
		Role:             RoleCustomer,
		Currency:         defaultCurrency,
		RefreshTokenHash: refreshToken.Hash,
		SessionExpiresAt: refreshExpiresAt,
		CreatedAt:        now,
	})
	cancel()
	if err != nil {
		return AuthResult{}, err
	}
	s.pruneExpiredSessions(ctx, now)

	s.logger.InfoContext(ctx, "user registered", slog.String("user_id", profile.User.ID.String()))
	return AuthResult{
		Profile: profile,
		Tokens: TokenPair{
			AccessToken:      accessToken,
			RefreshToken:     refreshToken.Raw,
			AccessExpiresAt:  accessExpiresAt,
			RefreshExpiresAt: refreshExpiresAt,
		},
	}, nil
}

func (s *Service) Login(ctx context.Context, req LoginRequest) (AuthResult, error) {
	email, emailErr := normalizeEmail(req.Email)
	validPasswordLength := validatePassword(req.Password) == nil

	dbCtx, cancel := context.WithTimeout(ctx, s.databaseTimeout)
	credentials, err := s.repository.GetCredentialsByEmail(dbCtx, email)
	cancel()
	userExists := err == nil && emailErr == nil
	if err != nil && !errors.Is(err, ErrUserNotFound) {
		return AuthResult{}, err
	}
	hash := s.dummyHash
	if userExists {
		hash = credentials.PasswordHash
	}
	passwordValid, verifyErr := s.passwords.Verify(req.Password, hash)
	if verifyErr != nil {
		if errors.Is(verifyErr, auth.ErrPasswordHasherBusy) {
			return AuthResult{}, ErrTemporarilyUnavailable
		}
		return AuthResult{}, fmt.Errorf("verify password: %w", verifyErr)
	}
	if !userExists || !validPasswordLength || !passwordValid || credentials.Profile.User.Status != StatusActive {
		return AuthResult{}, ErrInvalidCredentials
	}

	sessionID, err := uuid.NewRandom()
	if err != nil {
		return AuthResult{}, fmt.Errorf("generate session ID: %w", err)
	}
	refreshToken, err := auth.NewRefreshToken()
	if err != nil {
		return AuthResult{}, err
	}
	now := s.now().UTC()
	refreshExpiresAt := now.Add(s.refreshTTL)
	accessToken, accessExpiresAt, err := s.tokens.IssueAccess(
		credentials.Profile.User.ID,
		string(credentials.Profile.User.Role),
		now,
	)
	if err != nil {
		return AuthResult{}, err
	}

	dbCtx, cancel = context.WithTimeout(ctx, s.databaseTimeout)
	err = s.repository.CreateSession(dbCtx, CreateSessionParams{
		SessionID:        sessionID,
		UserID:           credentials.Profile.User.ID,
		RefreshTokenID:   refreshToken.ID,
		RefreshTokenHash: refreshToken.Hash,
		SessionExpiresAt: refreshExpiresAt,
		CreatedAt:        now,
	})
	cancel()
	if err != nil {
		return AuthResult{}, err
	}
	s.pruneExpiredSessions(ctx, now)

	s.logger.InfoContext(ctx, "user logged in", slog.String("user_id", credentials.Profile.User.ID.String()))
	return AuthResult{
		Profile: credentials.Profile,
		Tokens: TokenPair{
			AccessToken:      accessToken,
			RefreshToken:     refreshToken.Raw,
			AccessExpiresAt:  accessExpiresAt,
			RefreshExpiresAt: refreshExpiresAt,
		},
	}, nil
}

func (s *Service) Refresh(ctx context.Context, rawRefreshToken string) (AuthResult, error) {
	currentToken, err := auth.ParseRefreshToken(rawRefreshToken)
	if err != nil {
		return AuthResult{}, ErrInvalidRefreshToken
	}
	replacement, err := auth.NewRefreshToken()
	if err != nil {
		return AuthResult{}, err
	}
	dbCtx, cancel := context.WithTimeout(ctx, s.databaseTimeout)
	rotation, err := s.repository.RotateRefreshToken(dbCtx, RotateRefreshTokenParams{
		CurrentTokenID:     currentToken.ID,
		CurrentTokenHash:   currentToken.Hash,
		ReplacementTokenID: replacement.ID,
		ReplacementHash:    replacement.Hash,
		ReuseGrace:         s.refreshReuseGrace,
	})
	cancel()
	if errors.Is(err, ErrInvalidRefreshToken) {
		return AuthResult{}, ErrInvalidRefreshToken
	}
	if err != nil {
		return AuthResult{}, err
	}

	accessToken, accessExpiresAt, err := s.tokens.IssueAccess(
		rotation.Profile.User.ID,
		string(rotation.Profile.User.Role),
		rotation.RotatedAt,
	)
	if err != nil {
		return AuthResult{}, err
	}
	s.pruneExpiredSessions(ctx, rotation.RotatedAt)

	s.logger.InfoContext(ctx, "refresh token rotated", slog.String("user_id", rotation.Profile.User.ID.String()))
	return AuthResult{
		Profile: rotation.Profile,
		Tokens: TokenPair{
			AccessToken:      accessToken,
			RefreshToken:     replacement.Raw,
			AccessExpiresAt:  accessExpiresAt,
			RefreshExpiresAt: rotation.SessionExpiresAt,
		},
	}, nil
}

func (s *Service) Logout(ctx context.Context, rawRefreshToken string) error {
	refreshToken, err := auth.ParseRefreshToken(rawRefreshToken)
	if err != nil {
		return nil
	}
	dbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.databaseTimeout)
	err = s.repository.RevokeSession(dbCtx, RevokeSessionParams{
		RefreshTokenID:   refreshToken.ID,
		RefreshTokenHash: refreshToken.Hash,
	})
	cancel()
	if errors.Is(err, ErrInvalidRefreshToken) {
		return nil
	}
	return err
}

func (s *Service) GetProfile(ctx context.Context, userID uuid.UUID) (Profile, error) {
	dbCtx, cancel := context.WithTimeout(ctx, s.databaseTimeout)
	profile, err := s.repository.GetProfile(dbCtx, userID)
	cancel()
	if err != nil {
		return Profile{}, err
	}
	if profile.User.Status != StatusActive {
		return Profile{}, ErrUserDisabled
	}
	return profile, nil
}

func (s *Service) pruneExpiredSessions(ctx context.Context, before time.Time) {
	dbCtx, cancel := context.WithTimeout(ctx, s.databaseTimeout)
	defer cancel()
	deleted, err := s.repository.DeleteExpiredSessions(dbCtx, before, 100)
	if err != nil {
		if ctx.Err() == nil {
			s.logger.WarnContext(ctx, "expired auth session cleanup failed", slog.Any("error", err))
		}
		return
	}
	if deleted > 0 {
		s.logger.InfoContext(ctx, "expired auth sessions deleted", slog.Int64("count", deleted))
	}
}

func normalizeEmail(raw string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(raw))
	if len(normalized) < 3 || len(normalized) > 254 || !utf8.ValidString(normalized) {
		return "", ErrInvalidEmail
	}
	address, err := mail.ParseAddress(normalized)
	if err != nil || address.Address != normalized || strings.ContainsAny(normalized, "<>\r\n") {
		return "", ErrInvalidEmail
	}
	return normalized, nil
}

func normalizeDisplayName(raw string) (string, error) {
	normalized := strings.TrimSpace(raw)
	length := utf8.RuneCountInString(normalized)
	if !utf8.ValidString(normalized) || length < 1 || length > 100 {
		return "", ErrInvalidDisplayName
	}
	return normalized, nil
}

func validatePassword(password string) error {
	if len(password) < minimumPasswordLength || len(password) > maximumPasswordLength {
		return ErrInvalidPassword
	}
	return nil
}

type identityIDs struct {
	userID    uuid.UUID
	accountID uuid.UUID
	sessionID uuid.UUID
}

func newIdentityIDs() (identityIDs, error) {
	userID, err := uuid.NewRandom()
	if err != nil {
		return identityIDs{}, fmt.Errorf("generate user ID: %w", err)
	}
	accountID, err := uuid.NewRandom()
	if err != nil {
		return identityIDs{}, fmt.Errorf("generate account ID: %w", err)
	}
	sessionID, err := uuid.NewRandom()
	if err != nil {
		return identityIDs{}, fmt.Errorf("generate session ID: %w", err)
	}
	return identityIDs{userID: userID, accountID: accountID, sessionID: sessionID}, nil
}
