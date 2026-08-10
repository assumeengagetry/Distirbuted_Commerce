package user

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/assumeengagetry/distributed-commerce/internal/auth"
)

const serviceTestTokenKey = "1212121212121212121212121212121212121212121212121212121212121212"

func TestServiceRegister(t *testing.T) {
	t.Parallel()

	var captured CreateUserParams
	repository := &fakeRepository{
		createUser: func(_ context.Context, params CreateUserParams) (Profile, error) {
			captured = params
			return profileFromCreateParams(params), nil
		},
	}
	service, tokenManager, now := newTestService(t, repository, &fakePasswordManager{})

	result, err := service.Register(t.Context(), RegisterRequest{
		Email: "  Alice@Example.COM ", Password: "a-secure-password", DisplayName: " Alice ",
	})
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	if captured.Email != "alice@example.com" || captured.DisplayName != "Alice" {
		t.Errorf("normalized registration = (%q, %q)", captured.Email, captured.DisplayName)
	}
	if captured.Role != RoleCustomer || captured.Currency != "USD" || captured.PasswordHash == "a-secure-password" {
		t.Errorf("registration params = %+v", captured)
	}
	if captured.UserID == uuid.Nil || captured.AccountID == uuid.Nil || captured.SessionID == uuid.Nil {
		t.Error("registration contains nil IDs")
	}
	if !captured.SessionExpiresAt.Equal(now.Add(7 * 24 * time.Hour)) {
		t.Errorf("session expiry = %s", captured.SessionExpiresAt)
	}
	if _, err := auth.ParseRefreshToken(result.Tokens.RefreshToken); err != nil {
		t.Fatalf("refresh token parse error = %v", err)
	}
	principal, err := tokenManager.ParseAccess(result.Tokens.AccessToken, now)
	if err != nil || principal.UserID != captured.UserID || principal.Role != string(RoleCustomer) {
		t.Fatalf("access principal = (%+v, %v)", principal, err)
	}
}

func TestServiceRegisterValidation(t *testing.T) {
	t.Parallel()

	repository := &fakeRepository{
		createUser: func(context.Context, CreateUserParams) (Profile, error) {
			return Profile{}, errors.New("repository called for invalid registration")
		},
	}
	service, _, _ := newTestService(t, repository, &fakePasswordManager{})
	tests := []struct {
		name string
		req  RegisterRequest
		want error
	}{
		{name: "email", req: RegisterRequest{Email: "invalid", Password: "a-secure-password", DisplayName: "Alice"}, want: ErrInvalidEmail},
		{name: "password", req: RegisterRequest{Email: "a@example.com", Password: "short", DisplayName: "Alice"}, want: ErrInvalidPassword},
		{name: "display name", req: RegisterRequest{Email: "a@example.com", Password: "a-secure-password", DisplayName: " "}, want: ErrInvalidDisplayName},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := service.Register(t.Context(), tt.req); !errors.Is(err, tt.want) {
				t.Fatalf("Register() error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestServiceRegisterPropagatesDuplicateEmail(t *testing.T) {
	t.Parallel()

	repository := &fakeRepository{
		createUser: func(context.Context, CreateUserParams) (Profile, error) {
			return Profile{}, ErrEmailAlreadyExists
		},
	}
	service, _, _ := newTestService(t, repository, &fakePasswordManager{})
	_, err := service.Register(t.Context(), RegisterRequest{
		Email: "a@example.com", Password: "a-secure-password", DisplayName: "Alice",
	})
	if !errors.Is(err, ErrEmailAlreadyExists) {
		t.Fatalf("Register() error = %v, want ErrEmailAlreadyExists", err)
	}
}

func TestServiceLogin(t *testing.T) {
	t.Parallel()

	profile := testProfile(StatusActive)
	var session CreateSessionParams
	repository := &fakeRepository{
		getCredentials: func(context.Context, string) (Credentials, error) {
			return Credentials{Profile: profile, PasswordHash: "hash:correct-password"}, nil
		},
		createSession: func(_ context.Context, params CreateSessionParams) error {
			session = params
			return nil
		},
	}
	service, tokenManager, now := newTestService(t, repository, &fakePasswordManager{})
	result, err := service.Login(t.Context(), LoginRequest{Email: "USER@EXAMPLE.COM", Password: "correct-password"})
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}
	if session.UserID != profile.User.ID || session.SessionID == uuid.Nil {
		t.Errorf("created session = %+v", session)
	}
	principal, err := tokenManager.ParseAccess(result.Tokens.AccessToken, now)
	if err != nil || principal.UserID != profile.User.ID {
		t.Fatalf("access principal = (%+v, %v)", principal, err)
	}
}

func TestServiceLoginUsesDummyHashForUnknownUser(t *testing.T) {
	t.Parallel()

	passwords := &fakePasswordManager{}
	repository := &fakeRepository{
		getCredentials: func(context.Context, string) (Credentials, error) {
			return Credentials{}, ErrUserNotFound
		},
	}
	service, _, _ := newTestService(t, repository, passwords)
	_, err := service.Login(t.Context(), LoginRequest{Email: "missing@example.com", Password: "candidate-password"})
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("Login() error = %v, want ErrInvalidCredentials", err)
	}
	if passwords.lastVerifiedHash != "hash:invalid-user-password-value" {
		t.Fatalf("verified hash = %q, want dummy hash", passwords.lastVerifiedHash)
	}
}

func TestServiceLoginRejectsDisabledUser(t *testing.T) {
	t.Parallel()

	profile := testProfile(StatusDisabled)
	repository := &fakeRepository{
		getCredentials: func(context.Context, string) (Credentials, error) {
			return Credentials{Profile: profile, PasswordHash: "hash:correct-password"}, nil
		},
	}
	service, _, _ := newTestService(t, repository, &fakePasswordManager{})
	_, err := service.Login(t.Context(), LoginRequest{Email: profile.User.Email, Password: "correct-password"})
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("Login() error = %v, want ErrInvalidCredentials", err)
	}
}

func TestServiceLoginRejectsWhenPasswordHasherIsSaturated(t *testing.T) {
	t.Parallel()

	profile := testProfile(StatusActive)
	repository := &fakeRepository{
		getCredentials: func(context.Context, string) (Credentials, error) {
			return Credentials{Profile: profile, PasswordHash: "hash:correct-password"}, nil
		},
	}
	passwords := &fakePasswordManager{verifyErr: auth.ErrPasswordHasherBusy}
	service, _, _ := newTestService(t, repository, passwords)
	_, err := service.Login(t.Context(), LoginRequest{Email: profile.User.Email, Password: "correct-password"})
	if !errors.Is(err, ErrTemporarilyUnavailable) {
		t.Fatalf("Login() error = %v, want ErrTemporarilyUnavailable", err)
	}
}

func TestServiceRefresh(t *testing.T) {
	t.Parallel()

	current, err := auth.NewRefreshToken()
	if err != nil {
		t.Fatalf("NewRefreshToken() error = %v", err)
	}
	profile := testProfile(StatusActive)
	sessionExpiry := time.Date(2026, time.August, 17, 12, 0, 0, 0, time.UTC)
	var rotation RotateRefreshTokenParams
	repository := &fakeRepository{
		rotateRefresh: func(_ context.Context, params RotateRefreshTokenParams) (RotationResult, error) {
			rotation = params
			return RotationResult{Profile: profile, SessionExpiresAt: sessionExpiry, RotatedAt: sessionExpiry.Add(-24 * time.Hour)}, nil
		},
	}
	service, _, _ := newTestService(t, repository, &fakePasswordManager{})
	result, err := service.Refresh(t.Context(), current.Raw)
	if err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if rotation.CurrentTokenID != current.ID || rotation.ReplacementTokenID == uuid.Nil {
		t.Errorf("rotation params = %+v", rotation)
	}
	if result.Tokens.RefreshToken == current.Raw || !result.Tokens.RefreshExpiresAt.Equal(sessionExpiry) {
		t.Errorf("rotated token result = %+v", result.Tokens)
	}
}

func TestServiceLogoutIsIdempotent(t *testing.T) {
	t.Parallel()

	var revokeCalls int
	repository := &fakeRepository{
		revokeSession: func(context.Context, RevokeSessionParams) error {
			revokeCalls++
			return ErrInvalidRefreshToken
		},
	}
	service, _, _ := newTestService(t, repository, &fakePasswordManager{})
	if err := service.Logout(t.Context(), "malformed"); err != nil {
		t.Fatalf("Logout(malformed) error = %v", err)
	}
	token, err := auth.NewRefreshToken()
	if err != nil {
		t.Fatalf("NewRefreshToken() error = %v", err)
	}
	if err := service.Logout(t.Context(), token.Raw); err != nil {
		t.Fatalf("Logout(valid shape) error = %v", err)
	}
	if revokeCalls != 1 {
		t.Fatalf("revoke calls = %d, want 1", revokeCalls)
	}
}

func TestServiceLogoutCompletesAfterClientCancellation(t *testing.T) {
	t.Parallel()

	var repositoryContextErr error
	repository := &fakeRepository{
		revokeSession: func(ctx context.Context, _ RevokeSessionParams) error {
			repositoryContextErr = ctx.Err()
			return nil
		},
	}
	service, _, _ := newTestService(t, repository, &fakePasswordManager{})
	refreshToken, err := auth.NewRefreshToken()
	if err != nil {
		t.Fatalf("NewRefreshToken() error = %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := service.Logout(ctx, refreshToken.Raw); err != nil {
		t.Fatalf("Logout() error = %v", err)
	}
	if repositoryContextErr != nil {
		t.Fatalf("repository logout context error = %v, want detached bounded context", repositoryContextErr)
	}
}

func TestServiceGetProfileRejectsDisabledUser(t *testing.T) {
	t.Parallel()

	profile := testProfile(StatusDisabled)
	repository := &fakeRepository{
		getProfile: func(context.Context, uuid.UUID) (Profile, error) { return profile, nil },
	}
	service, _, _ := newTestService(t, repository, &fakePasswordManager{})
	if _, err := service.GetProfile(t.Context(), profile.User.ID); !errors.Is(err, ErrUserDisabled) {
		t.Fatalf("GetProfile() error = %v, want ErrUserDisabled", err)
	}
}

func TestServiceBoundsDatabaseOperations(t *testing.T) {
	t.Parallel()

	repository := &fakeRepository{
		getProfile: func(ctx context.Context, _ uuid.UUID) (Profile, error) {
			<-ctx.Done()
			return Profile{}, ctx.Err()
		},
	}
	tokens, err := auth.NewTokenManager(serviceTestTokenKey, "test-user-service", 15*time.Minute, 30*time.Second)
	if err != nil {
		t.Fatalf("NewTokenManager() error = %v", err)
	}
	service, err := NewService(
		repository,
		&fakePasswordManager{},
		tokens,
		slog.New(slog.NewJSONHandler(io.Discard, nil)),
		ServiceConfig{
			RefreshTTL: 24 * time.Hour, RefreshReuseGrace: time.Second,
			DatabaseTimeout: 10 * time.Millisecond, Now: time.Now,
		},
	)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	startedAt := time.Now()
	_, err = service.GetProfile(t.Context(), uuid.New())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("GetProfile() error = %v, want context deadline", err)
	}
	if elapsed := time.Since(startedAt); elapsed > time.Second {
		t.Fatalf("database deadline took %s", elapsed)
	}
}

type fakePasswordManager struct {
	lastVerifiedHash string
	hashErr          error
	verifyErr        error
}

func (f *fakePasswordManager) Hash(password string) (string, error) {
	if f.hashErr != nil {
		return "", f.hashErr
	}
	return "hash:" + password, nil
}

func (f *fakePasswordManager) Verify(password, encodedHash string) (bool, error) {
	f.lastVerifiedHash = encodedHash
	if f.verifyErr != nil {
		return false, f.verifyErr
	}
	return encodedHash == "hash:"+password, nil
}

type fakeRepository struct {
	createUser     func(context.Context, CreateUserParams) (Profile, error)
	getCredentials func(context.Context, string) (Credentials, error)
	createSession  func(context.Context, CreateSessionParams) error
	rotateRefresh  func(context.Context, RotateRefreshTokenParams) (RotationResult, error)
	revokeSession  func(context.Context, RevokeSessionParams) error
	getProfile     func(context.Context, uuid.UUID) (Profile, error)
}

func (f *fakeRepository) CreateUser(ctx context.Context, params CreateUserParams) (Profile, error) {
	return f.createUser(ctx, params)
}

func (f *fakeRepository) GetCredentialsByEmail(ctx context.Context, email string) (Credentials, error) {
	if f.getCredentials == nil {
		return Credentials{}, ErrUserNotFound
	}
	return f.getCredentials(ctx, email)
}

func (f *fakeRepository) CreateSession(ctx context.Context, params CreateSessionParams) error {
	if f.createSession == nil {
		return nil
	}
	return f.createSession(ctx, params)
}

func (f *fakeRepository) RotateRefreshToken(ctx context.Context, params RotateRefreshTokenParams) (RotationResult, error) {
	return f.rotateRefresh(ctx, params)
}

func (f *fakeRepository) RevokeSession(ctx context.Context, params RevokeSessionParams) error {
	if f.revokeSession == nil {
		return nil
	}
	return f.revokeSession(ctx, params)
}

func (f *fakeRepository) GetProfile(ctx context.Context, userID uuid.UUID) (Profile, error) {
	return f.getProfile(ctx, userID)
}

func (f *fakeRepository) DeleteExpiredSessions(context.Context, time.Time, int32) (int64, error) {
	return 0, nil
}

func newTestService(t *testing.T, repository Repository, passwords PasswordManager) (*Service, *auth.TokenManager, time.Time) {
	t.Helper()
	now := time.Date(2026, time.August, 10, 12, 0, 0, 0, time.UTC)
	tokens, err := auth.NewTokenManager(serviceTestTokenKey, "test-user-service", 15*time.Minute, 30*time.Second)
	if err != nil {
		t.Fatalf("NewTokenManager() error = %v", err)
	}
	service, err := NewService(
		repository,
		passwords,
		tokens,
		slog.New(slog.NewJSONHandler(io.Discard, nil)),
		ServiceConfig{
			RefreshTTL: 7 * 24 * time.Hour, RefreshReuseGrace: 5 * time.Second,
			DatabaseTimeout: time.Second, Now: func() time.Time { return now },
		},
	)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	return service, tokens, now
}

func profileFromCreateParams(params CreateUserParams) Profile {
	return Profile{
		User: User{
			ID: params.UserID, Email: params.Email, DisplayName: params.DisplayName, Role: params.Role,
			Status: StatusActive, CreatedAt: params.CreatedAt, UpdatedAt: params.CreatedAt,
		},
		Account: Account{
			ID: params.AccountID, UserID: params.UserID, Currency: params.Currency,
			CreatedAt: params.CreatedAt, UpdatedAt: params.CreatedAt,
		},
	}
}

func testProfile(status Status) Profile {
	now := time.Date(2026, time.August, 10, 12, 0, 0, 0, time.UTC)
	userID := uuid.New()
	return Profile{
		User: User{
			ID: userID, Email: "user@example.com", DisplayName: "User", Role: RoleCustomer,
			Status: status, CreatedAt: now, UpdatedAt: now,
		},
		Account: Account{
			ID: uuid.New(), UserID: userID, Currency: "USD", Balance: 0, CreatedAt: now, UpdatedAt: now,
		},
	}
}
