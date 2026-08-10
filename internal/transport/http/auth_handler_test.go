package httptransport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/assumeengagetry/distributed-commerce/internal/auth"
	"github.com/assumeengagetry/distributed-commerce/internal/user"
)

func TestRegisterHandler(t *testing.T) {
	t.Parallel()

	result := transportAuthResult()
	var captured user.RegisterRequest
	service := &stubUserService{register: func(_ context.Context, req user.RegisterRequest) (user.AuthResult, error) {
		captured = req
		return result, nil
	}}
	router := routerWithService(t, service, stubTokenVerifier{}, RateLimitConfig{
		RequestsPerSecond: 100, Burst: 10, EntryTTL: time.Minute, MaxEntries: 100,
	})

	requestID := uuid.NewString()
	request := httptest.NewRequest(http.MethodPost, "/v1/auth/register", strings.NewReader(
		`{"email":"alice@example.com","password":"a-secure-password","display_name":"Alice"}`,
	))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(requestIDHeader, requestID)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusCreated, response.Body.String())
	}
	if response.Header().Get(requestIDHeader) != requestID {
		t.Errorf("response request ID = %q, want %q", response.Header().Get(requestIDHeader), requestID)
	}
	if captured.Email != "alice@example.com" || captured.Password != "a-secure-password" || captured.DisplayName != "Alice" {
		t.Errorf("captured request = %+v", captured)
	}
	if strings.Contains(response.Body.String(), "password\"") {
		t.Fatalf("response exposes password field: %s", response.Body.String())
	}
	var body authResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if body.User.ID != result.Profile.User.ID || body.Account.Balance != 0 || body.Tokens.TokenType != "Bearer" {
		t.Errorf("response = %+v", body)
	}
}

func TestAuthHandlersRejectInvalidBodies(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		body        string
		contentType string
		wantStatus  int
		wantCode    string
	}{
		{name: "malformed", body: `{`, contentType: "application/json", wantStatus: 400, wantCode: "INVALID_REQUEST"},
		{name: "unknown field", body: `{"email":"a@example.com","password":"a-secure-password","display_name":"A","role":"admin"}`, contentType: "application/json", wantStatus: 400, wantCode: "INVALID_REQUEST"},
		{name: "trailing JSON", body: `{} {}`, contentType: "application/json", wantStatus: 400, wantCode: "INVALID_REQUEST"},
		{name: "unsupported media", body: `{}`, contentType: "text/plain", wantStatus: 415, wantCode: "UNSUPPORTED_MEDIA_TYPE"},
		{name: "oversized", body: `{"email":"` + strings.Repeat("a", maximumJSONBodyBytes) + `"}`, contentType: "application/json", wantStatus: 413, wantCode: "REQUEST_TOO_LARGE"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			router := routerWithService(t, &stubUserService{}, stubTokenVerifier{}, RateLimitConfig{
				RequestsPerSecond: 100, Burst: 10, EntryTTL: time.Minute, MaxEntries: 100,
			})
			request := httptest.NewRequest(http.MethodPost, "/v1/auth/register", strings.NewReader(tt.body))
			request.Header.Set("Content-Type", tt.contentType)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)

			assertAPIError(t, response, tt.wantStatus, tt.wantCode)
		})
	}
}

func TestAuthHandlerMapsDomainErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{name: "invalid email", err: user.ErrInvalidEmail, wantStatus: 400, wantCode: "INVALID_EMAIL"},
		{name: "invalid password", err: user.ErrInvalidPassword, wantStatus: 400, wantCode: "INVALID_PASSWORD"},
		{name: "duplicate", err: user.ErrEmailAlreadyExists, wantStatus: 409, wantCode: "EMAIL_ALREADY_EXISTS"},
		{name: "credentials", err: user.ErrInvalidCredentials, wantStatus: 401, wantCode: "INVALID_CREDENTIALS"},
		{name: "temporarily unavailable", err: user.ErrTemporarilyUnavailable, wantStatus: 503, wantCode: "SERVICE_UNAVAILABLE"},
		{name: "internal", err: errors.New("database secret detail"), wantStatus: 500, wantCode: "INTERNAL_ERROR"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			service := &stubUserService{register: func(context.Context, user.RegisterRequest) (user.AuthResult, error) {
				return user.AuthResult{}, tt.err
			}}
			router := routerWithService(t, service, stubTokenVerifier{}, RateLimitConfig{
				RequestsPerSecond: 100, Burst: 10, EntryTTL: time.Minute, MaxEntries: 100,
			})
			request := httptest.NewRequest(http.MethodPost, "/v1/auth/register", strings.NewReader(
				`{"email":"alice@example.com","password":"a-secure-password","display_name":"Alice"}`,
			))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)

			assertAPIError(t, response, tt.wantStatus, tt.wantCode)
			if strings.Contains(response.Body.String(), "database secret detail") {
				t.Fatalf("response exposes internal error: %s", response.Body.String())
			}
		})
	}
}

func TestRefreshAndLogoutHandlers(t *testing.T) {
	t.Parallel()

	result := transportAuthResult()
	var refreshed, loggedOut string
	service := &stubUserService{
		refresh: func(_ context.Context, token string) (user.AuthResult, error) {
			refreshed = token
			return result, nil
		},
		logout: func(_ context.Context, token string) error {
			loggedOut = token
			return nil
		},
	}
	router := routerWithService(t, service, stubTokenVerifier{}, RateLimitConfig{
		RequestsPerSecond: 100, Burst: 10, EntryTTL: time.Minute, MaxEntries: 100,
	})

	for _, test := range []struct {
		path       string
		wantStatus int
	}{
		{path: "/v1/auth/refresh", wantStatus: http.StatusOK},
		{path: "/v1/auth/logout", wantStatus: http.StatusNoContent},
	} {
		request := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(`{"refresh_token":"refresh-value"}`))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != test.wantStatus {
			t.Fatalf("%s status = %d, want %d; body=%s", test.path, response.Code, test.wantStatus, response.Body.String())
		}
	}
	if refreshed != "refresh-value" || loggedOut != "refresh-value" {
		t.Errorf("service tokens = (%q, %q)", refreshed, loggedOut)
	}
}

func TestAuthenticationMiddlewareAndCurrentUser(t *testing.T) {
	t.Parallel()

	profile := transportProfile()
	principal := auth.Principal{UserID: profile.User.ID, Role: string(profile.User.Role), TokenID: uuid.New()}
	service := &stubUserService{getProfile: func(_ context.Context, userID uuid.UUID) (user.Profile, error) {
		if userID != profile.User.ID {
			t.Errorf("GetProfile user ID = %s, want %s", userID, profile.User.ID)
		}
		return profile, nil
	}}
	router := routerWithService(t, service, stubTokenVerifier{principal: principal}, RateLimitConfig{
		RequestsPerSecond: 100, Burst: 10, EntryTTL: time.Minute, MaxEntries: 100,
	})

	request := httptest.NewRequest(http.MethodGet, "/v1/users/me", nil)
	request.Header.Set("Authorization", "bearer valid-token")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body.String())
	}

	for _, headers := range [][]string{nil, {"Basic value"}, {"Bearer one", "Bearer two"}} {
		request := httptest.NewRequest(http.MethodGet, "/v1/users/me", nil)
		for _, header := range headers {
			request.Header.Add("Authorization", header)
		}
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		assertAPIError(t, response, http.StatusUnauthorized, "UNAUTHORIZED")
	}
}

func TestAuthorizationMiddleware(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		role       string
		wantStatus int
	}{
		{role: string(user.RoleAdmin), wantStatus: http.StatusNoContent},
		{role: string(user.RoleCustomer), wantStatus: http.StatusForbidden},
	} {
		router := gin.New()
		router.Use(requestIDMiddleware(), func(c *gin.Context) {
			ctx := auth.ContextWithPrincipal(c.Request.Context(), auth.Principal{UserID: uuid.New(), Role: tt.role, TokenID: uuid.New()})
			c.Request = c.Request.WithContext(ctx)
			c.Next()
		}, requireRoles(string(user.RoleAdmin)))
		router.GET("/admin", func(c *gin.Context) { c.Status(http.StatusNoContent) })
		request := httptest.NewRequest(http.MethodGet, "/admin", nil)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != tt.wantStatus {
			t.Errorf("role %q status = %d, want %d", tt.role, response.Code, tt.wantStatus)
		}
	}
}

func TestAuthRateLimit(t *testing.T) {
	t.Parallel()

	router := routerWithService(t, &stubUserService{}, stubTokenVerifier{}, RateLimitConfig{
		RequestsPerSecond: 0.1, Burst: 1, EntryTTL: time.Minute, MaxEntries: 100,
	})
	for requestNumber := 1; requestNumber <= 2; requestNumber++ {
		request := httptest.NewRequest(http.MethodPost, "/v1/auth/login", strings.NewReader(`{"email":"a@example.com","password":"a-secure-password"}`))
		request.Header.Set("Content-Type", "application/json")
		request.RemoteAddr = "192.0.2.1:1234"
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if requestNumber == 1 && response.Code != http.StatusOK {
			t.Fatalf("first request status = %d, want 200", response.Code)
		}
		if requestNumber == 2 {
			assertAPIError(t, response, http.StatusTooManyRequests, "RATE_LIMITED")
			if response.Header().Get("Retry-After") != "10" {
				t.Errorf("Retry-After = %q, want 10", response.Header().Get("Retry-After"))
			}
		}
	}
}

func TestIPRateLimiterBoundsCapacityWithOverflowBucket(t *testing.T) {
	t.Parallel()

	now := transportTestNow
	limiter, err := newIPRateLimiter(RateLimitConfig{
		RequestsPerSecond: 1, Burst: 1, EntryTTL: time.Minute, MaxEntries: 2,
	}, func() time.Time { return now })
	if err != nil {
		t.Fatalf("newIPRateLimiter() error = %v", err)
	}
	if !limiter.allow("192.0.2.1") || !limiter.allow("192.0.2.2") || !limiter.allow("192.0.2.3") {
		t.Fatal("initial or overflow-limited visitor was unexpectedly rejected")
	}
	if len(limiter.visitors) != 2 {
		t.Fatalf("visitor count = %d, want bounded count 2", len(limiter.visitors))
	}
	if limiter.allow("192.0.2.4") {
		t.Fatal("overflow bucket allowed address churn without replenishment")
	}
	now = now.Add(time.Second)
	if !limiter.allow("192.0.2.4") {
		t.Fatal("overflow bucket did not replenish")
	}
}

func TestRemoteIPAggregatesIPv6Prefix(t *testing.T) {
	t.Parallel()

	first := remoteIP("[2001:db8:abcd:12::1]:1000")
	second := remoteIP("[2001:db8:abcd:12::ffff]:2000")
	if first != second || first != "2001:db8:abcd:12::" {
		t.Fatalf("IPv6 prefixes = (%q, %q), want same /64", first, second)
	}
	if got := remoteIP("192.0.2.10:8080"); got != "192.0.2.10" {
		t.Fatalf("IPv4 remote IP = %q", got)
	}
}

func routerWithService(
	t *testing.T,
	service UserService,
	verifier AccessTokenVerifier,
	rateLimit RateLimitConfig,
) *gin.Engine {
	t.Helper()
	deps := baseTestDependencies(func(context.Context) error { return nil }, time.Second)
	deps.UserService = service
	deps.TokenVerifier = verifier
	deps.AuthRateLimit = rateLimit
	router, err := NewRouter(deps)
	if err != nil {
		t.Fatalf("NewRouter() error = %v", err)
	}
	return router
}

func assertAPIError(t *testing.T, response *httptest.ResponseRecorder, wantStatus int, wantCode string) {
	t.Helper()
	if response.Code != wantStatus {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, wantStatus, response.Body.String())
	}
	var body apiErrorEnvelope
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal error response: %v", err)
	}
	if body.Error.Code != wantCode || body.Error.RequestID == "" {
		t.Errorf("error response = %+v, want code %q and request ID", body.Error, wantCode)
	}
}

func transportAuthResult() user.AuthResult {
	profile := transportProfile()
	return user.AuthResult{
		Profile: profile,
		Tokens: user.TokenPair{
			AccessToken: "access-token", RefreshToken: "refresh-token",
			AccessExpiresAt: transportTestNow.Add(15 * time.Minute), RefreshExpiresAt: transportTestNow.Add(7 * 24 * time.Hour),
		},
	}
}

func transportProfile() user.Profile {
	userID := uuid.New()
	return user.Profile{
		User: user.User{
			ID: userID, Email: "alice@example.com", DisplayName: "Alice", Role: user.RoleCustomer,
			Status: user.StatusActive, CreatedAt: transportTestNow, UpdatedAt: transportTestNow,
		},
		Account: user.Account{
			ID: uuid.New(), UserID: userID, Currency: "USD", Balance: 0,
			CreatedAt: transportTestNow, UpdatedAt: transportTestNow,
		},
	}
}

func TestErrorResponseFormatting(t *testing.T) {
	t.Parallel()
	response := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(response)
	c.Set(requestIDKey, "request-id")
	writeAPIError(c, http.StatusTeapot, "CODE", fmt.Sprintf("message %d", 1))
	assertAPIError(t, response, http.StatusTeapot, "CODE")
}
