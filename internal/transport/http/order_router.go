package httptransport

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	commerce "github.com/assumeengagetry/distributed-commerce/internal/order"
)

type CommerceService interface {
	CreateProduct(context.Context, commerce.Actor, commerce.CreateProductRequest) (commerce.AdminProduct, error)
	UpdateProduct(context.Context, commerce.Actor, uuid.UUID, commerce.UpdateProductRequest) (commerce.AdminProduct, error)
	GetProduct(context.Context, uuid.UUID) (commerce.Product, error)
	GetAdminProduct(context.Context, commerce.Actor, uuid.UUID) (commerce.AdminProduct, error)
	ListProducts(context.Context, commerce.PageRequest) (commerce.ProductPage, error)
	AdjustInventory(context.Context, commerce.Actor, uuid.UUID, commerce.AdjustInventoryRequest) (commerce.Inventory, error)
	ListInventory(context.Context, commerce.Actor, commerce.PageRequest) (commerce.InventoryPage, error)
	CreateOrder(context.Context, commerce.Actor, commerce.CreateOrderRequest) (commerce.Order, error)
	ListOrders(context.Context, commerce.Actor, commerce.PageRequest) (commerce.OrderPage, error)
	GetOrder(context.Context, commerce.Actor, uuid.UUID) (commerce.Order, error)
}

type OrderDependencies struct {
	Logger           *slog.Logger
	ServiceName      string
	ReadinessCheck   func(context.Context) error
	ReadinessTimeout time.Duration
	CommerceService  CommerceService
	TokenVerifier    AccessTokenVerifier
	RateLimit        RateLimitConfig
	Now              func() time.Time
}

func NewOrderRouter(deps OrderDependencies) (*gin.Engine, error) {
	if deps.CommerceService == nil {
		return nil, fmt.Errorf("commerce service is required")
	}
	if deps.TokenVerifier == nil {
		return nil, fmt.Errorf("token verifier is required")
	}
	if deps.Now == nil {
		return nil, fmt.Errorf("clock is required")
	}
	router, err := newBaseRouter(baseDependencies{
		Logger: deps.Logger, ServiceName: deps.ServiceName, ReadinessCheck: deps.ReadinessCheck,
		ReadinessTimeout: deps.ReadinessTimeout,
	})
	if err != nil {
		return nil, err
	}
	ipLimiter, err := newIPRateLimiter(deps.RateLimit, deps.Now)
	if err != nil {
		return nil, fmt.Errorf("create commerce rate limiter: %w", err)
	}
	principalLimiter, err := newIPRateLimiter(deps.RateLimit, deps.Now)
	if err != nil {
		return nil, fmt.Errorf("create principal rate limiter: %w", err)
	}

	handler := commerceHandler{service: deps.CommerceService, logger: deps.Logger}
	products := router.Group("/v1/products", rateLimitMiddleware(ipLimiter))
	products.GET("", handler.listProducts)
	products.GET("/:product_id", handler.getProduct)

	protected := router.Group(
		"/v1",
		rateLimitMiddleware(ipLimiter),
		authenticate(deps.TokenVerifier, deps.Now),
		principalRateLimitMiddleware(principalLimiter),
	)
	protected.POST("/orders", requireRoles(commerce.RoleCustomer), handler.createOrder)
	protected.GET("/orders", handler.listOrders)
	protected.GET("/orders/:order_id", handler.getOrder)

	admin := protected.Group("/admin", requireRoles(commerce.RoleAdmin))
	admin.POST("/products", handler.createProduct)
	admin.GET("/products/:product_id", handler.getAdminProduct)
	admin.PATCH("/products/:product_id", handler.updateProduct)
	admin.GET("/inventory", handler.listInventory)
	admin.PATCH("/inventory/:product_id", handler.adjustInventory)

	return router, nil
}
