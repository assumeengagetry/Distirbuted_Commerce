package httptransport

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestHealthEndpoints(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		path       string
		check      func(context.Context) error
		wantStatus int
		wantBody   healthResponse
	}{
		{
			name: "liveness does not depend on postgres",
			path: "/healthz",
			check: func(context.Context) error {
				panic("liveness called the readiness check")
			},
			wantStatus: http.StatusOK,
			wantBody: healthResponse{
				Status:  "ok",
				Service: "user-service",
			},
		},
		{
			name: "ready",
			path: "/readyz",
			check: func(context.Context) error {
				return nil
			},
			wantStatus: http.StatusOK,
			wantBody: healthResponse{
				Status:  "ready",
				Service: "user-service",
				Checks:  map[string]string{"postgres": "up"},
			},
		},
		{
			name: "not ready",
			path: "/readyz",
			check: func(context.Context) error {
				return context.Canceled
			},
			wantStatus: http.StatusServiceUnavailable,
			wantBody: healthResponse{
				Status:  "not_ready",
				Service: "user-service",
				Checks:  map[string]string{"postgres": "down"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			router := newTestRouter(t, tt.check, time.Second)
			request := httptest.NewRequest(http.MethodGet, tt.path, nil)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)

			if response.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", response.Code, tt.wantStatus)
			}
			if got := response.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", got)
			}

			var body healthResponse
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatalf("unmarshal response: %v", err)
			}
			if body.Status != tt.wantBody.Status || body.Service != tt.wantBody.Service {
				t.Errorf("response = %+v, want %+v", body, tt.wantBody)
			}
			if body.Checks["postgres"] != tt.wantBody.Checks["postgres"] {
				t.Errorf("postgres check = %q, want %q", body.Checks["postgres"], tt.wantBody.Checks["postgres"])
			}
		})
	}
}

func TestReadinessEnforcesTimeout(t *testing.T) {
	t.Parallel()

	router := newTestRouter(t, func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}, 10*time.Millisecond)

	request := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusServiceUnavailable)
	}
}

func TestRecoveryDoesNotExposePanic(t *testing.T) {
	t.Parallel()

	router := newTestRouter(t, func(context.Context) error { return nil }, time.Second)
	router.GET("/panic", func(*gin.Context) {
		panic("sensitive internal detail")
	})

	request := httptest.NewRequest(http.MethodGet, "/panic", nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusInternalServerError)
	}
	if strings.Contains(response.Body.String(), "sensitive internal detail") {
		t.Fatalf("response exposes panic: %s", response.Body.String())
	}
}

func TestNewRouterValidatesDependencies(t *testing.T) {
	t.Parallel()

	check := func(context.Context) error { return nil }

	tests := []struct {
		name   string
		mutate func(*Dependencies)
	}{
		{name: "missing logger", mutate: func(deps *Dependencies) { deps.Logger = nil }},
		{name: "missing service name", mutate: func(deps *Dependencies) { deps.ServiceName = "" }},
		{name: "missing check", mutate: func(deps *Dependencies) { deps.ReadinessCheck = nil }},
		{name: "invalid timeout", mutate: func(deps *Dependencies) { deps.ReadinessTimeout = 0 }},
		{name: "missing user service", mutate: func(deps *Dependencies) { deps.UserService = nil }},
		{name: "missing verifier", mutate: func(deps *Dependencies) { deps.TokenVerifier = nil }},
		{name: "missing clock", mutate: func(deps *Dependencies) { deps.Now = nil }},
		{name: "invalid rate limit", mutate: func(deps *Dependencies) { deps.AuthRateLimit.Burst = 0 }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			deps := baseTestDependencies(check, time.Second)
			tt.mutate(&deps)
			if _, err := NewRouter(deps); err == nil {
				t.Fatal("NewRouter() error = nil, want an error")
			}
		})
	}
}

func newTestRouter(t *testing.T, check func(context.Context) error, timeout time.Duration) *gin.Engine {
	t.Helper()

	router, err := NewRouter(baseTestDependencies(check, timeout))
	if err != nil {
		t.Fatalf("NewRouter() error = %v", err)
	}
	return router
}
