//go:build integration

package httptransport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/assumeengagetry/distributed-commerce/internal/auth"
	"github.com/assumeengagetry/distributed-commerce/internal/config"
	"github.com/assumeengagetry/distributed-commerce/internal/database"
	store "github.com/assumeengagetry/distributed-commerce/internal/database/sqlc"
	"github.com/assumeengagetry/distributed-commerce/internal/user"
)

const integrationTokenKey = "3434343434343434343434343434343434343434343434343434343434343434"

func TestAuthenticationAPIIntegration(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	pool, err := database.Open(ctx, config.DatabaseConfig{
		URL: databaseURL, MaxConns: 10, MinIdleConns: 1,
		MaxConnLifetime: time.Hour, MaxConnLifetimeJitter: 5 * time.Minute,
		MaxConnIdleTime: 30 * time.Minute, HealthCheckPeriod: time.Minute, PingTimeout: 3 * time.Second,
		OperationTimeout: 5 * time.Second, LockTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	t.Cleanup(pool.Close)

	passwords, err := auth.NewPasswordHasher(auth.PasswordParams{
		MemoryKiB: 8192, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32, MaxConcurrency: 2,
	})
	if err != nil {
		t.Fatalf("NewPasswordHasher() error = %v", err)
	}
	tokens, err := auth.NewTokenManager(integrationTokenKey, "integration-user-service", 15*time.Minute, 30*time.Second)
	if err != nil {
		t.Fatalf("NewTokenManager() error = %v", err)
	}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	service, err := user.NewService(
		database.NewUserRepository(pool, 2*time.Second, 5*time.Second), passwords, tokens, logger,
		user.ServiceConfig{
			RefreshTTL: 24 * time.Hour, RefreshReuseGrace: 5 * time.Second,
			DatabaseTimeout: 5 * time.Second, Now: time.Now,
		},
	)
	if err != nil {
		t.Fatalf("user.NewService() error = %v", err)
	}
	queries := store.New(pool)
	router, err := NewRouter(Dependencies{
		Logger: logger, ServiceName: "user-service",
		ReadinessChecks:  map[string]func(context.Context) error{"postgres": func(ctx context.Context) error { _, err := queries.HealthCheck(ctx); return err }},
		ReadinessTimeout: 3 * time.Second,
		UserService:      service, TokenVerifier: tokens,
		AuthRateLimit: RateLimitConfig{RequestsPerSecond: 1000, Burst: 100, EntryTTL: time.Minute, MaxEntries: 100},
		Now:           time.Now,
	})
	if err != nil {
		t.Fatalf("NewRouter() error = %v", err)
	}

	email := fmt.Sprintf("api-%s@example.com", uuid.NewString())
	const password = "integration-password"
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		if _, err := pool.Exec(cleanupCtx, `DELETE FROM users WHERE email = $1`, email); err != nil {
			t.Errorf("delete API integration user: %v", err)
		}
	})

	register := performJSONRequest(t, router, http.MethodPost, "/v1/auth/register", map[string]string{
		"email": email, "password": password, "display_name": "API User",
	}, "")
	if register.Code != http.StatusCreated {
		t.Fatalf("register status = %d; body=%s", register.Code, register.Body.String())
	}
	registered := decodeAuthResponse(t, register)
	if registered.Account.Balance != 0 || registered.User.Role != user.RoleCustomer {
		t.Errorf("registered response = %+v", registered)
	}

	duplicate := performJSONRequest(t, router, http.MethodPost, "/v1/auth/register", map[string]string{
		"email": email, "password": password, "display_name": "API User",
	}, "")
	assertAPIError(t, duplicate, http.StatusConflict, "EMAIL_ALREADY_EXISTS")

	wrongLogin := performJSONRequest(t, router, http.MethodPost, "/v1/auth/login", map[string]string{
		"email": email, "password": "wrong-password-value",
	}, "")
	assertAPIError(t, wrongLogin, http.StatusUnauthorized, "INVALID_CREDENTIALS")

	login := performJSONRequest(t, router, http.MethodPost, "/v1/auth/login", map[string]string{
		"email": email, "password": password,
	}, "")
	if login.Code != http.StatusOK {
		t.Fatalf("login status = %d; body=%s", login.Code, login.Body.String())
	}
	loggedIn := decodeAuthResponse(t, login)

	me := performJSONRequest(t, router, http.MethodGet, "/v1/users/me", nil, loggedIn.Tokens.AccessToken)
	if me.Code != http.StatusOK {
		t.Fatalf("me status = %d; body=%s", me.Code, me.Body.String())
	}

	refresh := performJSONRequest(t, router, http.MethodPost, "/v1/auth/refresh", map[string]string{
		"refresh_token": loggedIn.Tokens.RefreshToken,
	}, "")
	if refresh.Code != http.StatusOK {
		t.Fatalf("refresh status = %d; body=%s", refresh.Code, refresh.Body.String())
	}
	rotated := decodeAuthResponse(t, refresh)
	if rotated.Tokens.RefreshToken == loggedIn.Tokens.RefreshToken {
		t.Fatal("refresh endpoint did not rotate the refresh token")
	}

	oldRefresh := performJSONRequest(t, router, http.MethodPost, "/v1/auth/refresh", map[string]string{
		"refresh_token": loggedIn.Tokens.RefreshToken,
	}, "")
	assertAPIError(t, oldRefresh, http.StatusUnauthorized, "INVALID_REFRESH_TOKEN")

	logout := performJSONRequest(t, router, http.MethodPost, "/v1/auth/logout", map[string]string{
		"refresh_token": rotated.Tokens.RefreshToken,
	}, "")
	if logout.Code != http.StatusNoContent {
		t.Fatalf("logout status = %d; body=%s", logout.Code, logout.Body.String())
	}
	afterLogout := performJSONRequest(t, router, http.MethodPost, "/v1/auth/refresh", map[string]string{
		"refresh_token": rotated.Tokens.RefreshToken,
	}, "")
	assertAPIError(t, afterLogout, http.StatusUnauthorized, "INVALID_REFRESH_TOKEN")

	var passwordStoredAsPlaintext bool
	if err := pool.QueryRow(ctx, `SELECT password_hash = $2 FROM users WHERE email = $1`, email, password).Scan(&passwordStoredAsPlaintext); err != nil {
		t.Fatalf("query password storage: %v", err)
	}
	if passwordStoredAsPlaintext {
		t.Fatal("database contains plaintext password")
	}
}

func performJSONRequest(t *testing.T, handler http.Handler, method, path string, body any, accessToken string) *httptest.ResponseRecorder {
	return performJSONRequestWithIdempotency(t, handler, method, path, body, accessToken, "")
}

func performJSONRequestWithIdempotency(
	t *testing.T,
	handler http.Handler,
	method, path string,
	body any,
	accessToken, idempotencyKey string,
) *httptest.ResponseRecorder {
	t.Helper()
	var requestBody bytes.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
		requestBody = *bytes.NewReader(encoded)
	}
	request := httptest.NewRequest(method, path, &requestBody)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if accessToken != "" {
		request.Header.Set("Authorization", "Bearer "+accessToken)
	}
	if idempotencyKey != "" {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func decodeAuthResponse(t *testing.T, response *httptest.ResponseRecorder) authResponse {
	t.Helper()
	var decoded authResponse
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode auth response: %v", err)
	}
	return decoded
}
