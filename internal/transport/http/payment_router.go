package httptransport

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/assumeengagetry/distributed-commerce/internal/payment"
)

type PaymentService interface {
	CreatePayment(context.Context, payment.Actor, payment.CreateRequest) (payment.Payment, error)
	GetPayment(context.Context, payment.Actor, uuid.UUID) (payment.Payment, error)
}

type PaymentDependencies struct {
	Logger           *slog.Logger
	ServiceName      string
	ReadinessChecks  map[string]func(context.Context) error
	ReadinessTimeout time.Duration
	PaymentService   PaymentService
	TokenVerifier    AccessTokenVerifier
	RateLimit        RateLimitConfig
	Now              func() time.Time
}

func NewPaymentRouter(deps PaymentDependencies) (*gin.Engine, error) {
	if deps.PaymentService == nil {
		return nil, fmt.Errorf("payment service is required")
	}
	if deps.TokenVerifier == nil {
		return nil, fmt.Errorf("token verifier is required")
	}
	if deps.Now == nil {
		return nil, fmt.Errorf("clock is required")
	}
	router, err := newBaseRouter(baseDependencies{
		Logger: deps.Logger, ServiceName: deps.ServiceName, ReadinessChecks: deps.ReadinessChecks,
		ReadinessTimeout: deps.ReadinessTimeout,
	})
	if err != nil {
		return nil, err
	}
	ipLimiter, err := newIPRateLimiter(deps.RateLimit, deps.Now)
	if err != nil {
		return nil, fmt.Errorf("create payment IP rate limiter: %w", err)
	}
	principalLimiter, err := newIPRateLimiter(deps.RateLimit, deps.Now)
	if err != nil {
		return nil, fmt.Errorf("create payment principal rate limiter: %w", err)
	}
	handler := paymentHandler{service: deps.PaymentService, logger: deps.Logger}
	protected := router.Group(
		"/v1",
		rateLimitMiddleware(ipLimiter),
		authenticate(deps.TokenVerifier),
		principalRateLimitMiddleware(principalLimiter),
	)
	protected.POST("/payments", requireRoles(payment.RoleCustomer), handler.create)
	protected.GET("/payments/:payment_id", handler.get)
	return router, nil
}
