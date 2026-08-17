package httptransport

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin"

	"github.com/assumeengagetry/distributed-commerce/internal/observability"
)

type Dependencies struct {
	Logger           *slog.Logger
	ServiceName      string
	ReadinessChecks  map[string]func(context.Context) error
	ReadinessTimeout time.Duration
	UserService      UserService
	TokenVerifier    AccessTokenVerifier
	AuthRateLimit    RateLimitConfig
	Now              func() time.Time
	Telemetry        observability.Providers
}

func NewRouter(deps Dependencies) (*gin.Engine, error) {
	if deps.UserService == nil {
		return nil, fmt.Errorf("user service is required")
	}
	if deps.TokenVerifier == nil {
		return nil, fmt.Errorf("token verifier is required")
	}
	if deps.Now == nil {
		return nil, fmt.Errorf("clock is required")
	}
	router, err := newBaseRouter(baseDependencies{
		Logger: deps.Logger, ServiceName: deps.ServiceName, ReadinessChecks: deps.ReadinessChecks,
		ReadinessTimeout: deps.ReadinessTimeout, Telemetry: deps.Telemetry,
	})
	if err != nil {
		return nil, err
	}
	authLimiter, err := newIPRateLimiter(deps.AuthRateLimit, deps.Now)
	if err != nil {
		return nil, fmt.Errorf("create auth rate limiter: %w", err)
	}

	authRoutes := router.Group("/v1/auth", rateLimitMiddleware(authLimiter))
	userHandler := authHandler{service: deps.UserService, logger: deps.Logger}
	authRoutes.POST("/register", userHandler.register)
	authRoutes.POST("/login", userHandler.login)
	authRoutes.POST("/refresh", userHandler.refresh)
	authRoutes.POST("/logout", userHandler.logout)

	protected := router.Group("/v1", authenticate(deps.TokenVerifier))
	protected.GET("/users/me", userHandler.me)

	return router, nil
}

type baseDependencies struct {
	Logger           *slog.Logger
	ServiceName      string
	ReadinessChecks  map[string]func(context.Context) error
	ReadinessTimeout time.Duration
	Telemetry        observability.Providers
}

func newBaseRouter(deps baseDependencies) (*gin.Engine, error) {
	if deps.Logger == nil {
		return nil, fmt.Errorf("logger is required")
	}
	if deps.ServiceName == "" {
		return nil, fmt.Errorf("service name is required")
	}
	if len(deps.ReadinessChecks) == 0 {
		return nil, fmt.Errorf("at least one readiness check is required")
	}
	for name, check := range deps.ReadinessChecks {
		if name == "" || check == nil {
			return nil, fmt.Errorf("readiness check names and functions are required")
		}
	}
	if deps.ReadinessTimeout <= 0 {
		return nil, fmt.Errorf("readiness timeout must be positive")
	}

	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.HandleMethodNotAllowed = true
	if err := router.SetTrustedProxies(nil); err != nil {
		return nil, fmt.Errorf("disable trusted proxies: %w", err)
	}
	if deps.Telemetry.Enabled() {
		router.Use(otelgin.Middleware(
			deps.ServiceName,
			otelgin.WithTracerProvider(deps.Telemetry.TracerProvider),
			otelgin.WithMeterProvider(deps.Telemetry.MeterProvider),
			otelgin.WithPropagators(observability.PublicHTTPPropagator()),
		))
	}
	router.Use(requestIDMiddleware(), securityHeaders(), recovery(deps.Logger))
	router.NoRoute(func(c *gin.Context) {
		writeAPIError(c, http.StatusNotFound, "ROUTE_NOT_FOUND", "route not found")
	})
	router.NoMethod(func(c *gin.Context) {
		writeAPIError(c, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
	})

	handler := healthHandler{
		serviceName: deps.ServiceName, readinessChecks: deps.ReadinessChecks, checkTimeout: deps.ReadinessTimeout,
	}
	handler.register(router)
	return router, nil
}

func securityHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("X-Content-Type-Options", "nosniff")
		if c.Request.TLS != nil {
			c.Header("Strict-Transport-Security", "max-age=31536000")
		}
		c.Next()
	}
}

func recovery(logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if recovered := recover(); recovered != nil {
				logger.ErrorContext(
					c.Request.Context(),
					"panic recovered",
					slog.Any("panic", recovered),
					slog.String("stack", string(debug.Stack())),
					slog.String("request_id", requestIDFromContext(c)),
				)
				writeAPIError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "internal server error")
			}
		}()

		c.Next()
	}
}
