package openapi_test

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/assumeengagetry/distributed-commerce/internal/auth"
	"github.com/assumeengagetry/distributed-commerce/internal/order"
	"github.com/assumeengagetry/distributed-commerce/internal/payment"
	"github.com/assumeengagetry/distributed-commerce/internal/transport/http"
	"github.com/assumeengagetry/distributed-commerce/internal/user"
)

func TestOpenAPIPathsMatchConcreteGinRouters(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	readiness := map[string]func(context.Context) error{"dependency": func(context.Context) error { return nil }}
	rateLimit := httptransport.RateLimitConfig{
		RequestsPerSecond: 1, Burst: 1, EntryTTL: time.Minute, MaxEntries: 100,
	}
	userRouter, err := httptransport.NewRouter(httptransport.Dependencies{
		Logger: logger, ServiceName: "user-service", ReadinessChecks: readiness,
		ReadinessTimeout: time.Second, UserService: routeUserService{},
		TokenVerifier: routeTokenVerifier{}, AuthRateLimit: rateLimit, Now: time.Now,
	})
	if err != nil {
		t.Fatalf("NewRouter() error = %v", err)
	}
	orderRouter, err := httptransport.NewOrderRouter(httptransport.OrderDependencies{
		Logger: logger, ServiceName: "order-service", ReadinessChecks: readiness,
		ReadinessTimeout: time.Second, CommerceService: routeCommerceService{},
		TokenVerifier: routeTokenVerifier{}, RateLimit: rateLimit, Now: time.Now,
	})
	if err != nil {
		t.Fatalf("NewOrderRouter() error = %v", err)
	}
	paymentRouter, err := httptransport.NewPaymentRouter(httptransport.PaymentDependencies{
		Logger: logger, ServiceName: "payment-service", ReadinessChecks: readiness,
		ReadinessTimeout: time.Second, PaymentService: routePaymentService{},
		TokenVerifier: routeTokenVerifier{}, RateLimit: rateLimit, Now: time.Now,
	})
	if err != nil {
		t.Fatalf("NewPaymentRouter() error = %v", err)
	}

	actual := make(map[string]struct{})
	for _, router := range []*gin.Engine{userRouter, orderRouter, paymentRouter} {
		for _, route := range router.Routes() {
			actual[route.Method+" "+normalizeGinPath(route.Path)] = struct{}{}
		}
	}
	for key := range expectedOperations() {
		if _, ok := actual[key]; !ok {
			t.Errorf("concrete routers are missing OpenAPI operation %s", key)
		}
	}
	if len(actual) != len(expectedOperations()) {
		for key := range actual {
			if _, expected := expectedOperations()[key]; !expected {
				t.Errorf("concrete routers expose undocumented operation %s", key)
			}
		}
	}
}

func normalizeGinPath(path string) string {
	parts := strings.Split(path, "/")
	for index, part := range parts {
		if strings.HasPrefix(part, ":") {
			parts[index] = "{" + strings.TrimPrefix(part, ":") + "}"
		}
	}
	return strings.Join(parts, "/")
}

type routeTokenVerifier struct{}

func (routeTokenVerifier) VerifyAccess(context.Context, string) (auth.Principal, error) {
	return auth.Principal{UserID: uuid.New(), Role: string(order.RoleAdmin)}, nil
}

type routeUserService struct{}

func (routeUserService) Register(context.Context, user.RegisterRequest) (user.AuthResult, error) {
	return user.AuthResult{}, nil
}
func (routeUserService) Login(context.Context, user.LoginRequest) (user.AuthResult, error) {
	return user.AuthResult{}, nil
}
func (routeUserService) Refresh(context.Context, string) (user.AuthResult, error) {
	return user.AuthResult{}, nil
}
func (routeUserService) Logout(context.Context, string) error { return nil }
func (routeUserService) GetProfile(context.Context, uuid.UUID) (user.Profile, error) {
	return user.Profile{}, nil
}

type routeCommerceService struct{}

func (routeCommerceService) CreateProduct(context.Context, order.Actor, order.CreateProductRequest) (order.AdminProduct, error) {
	return order.AdminProduct{}, nil
}
func (routeCommerceService) UpdateProduct(context.Context, order.Actor, uuid.UUID, order.UpdateProductRequest) (order.AdminProduct, error) {
	return order.AdminProduct{}, nil
}
func (routeCommerceService) GetProduct(context.Context, uuid.UUID) (order.Product, error) {
	return order.Product{}, nil
}
func (routeCommerceService) GetAdminProduct(context.Context, order.Actor, uuid.UUID) (order.AdminProduct, error) {
	return order.AdminProduct{}, nil
}
func (routeCommerceService) ListProducts(context.Context, order.PageRequest) (order.ProductPage, error) {
	return order.ProductPage{}, nil
}
func (routeCommerceService) AdjustInventory(context.Context, order.Actor, uuid.UUID, order.AdjustInventoryRequest) (order.Inventory, error) {
	return order.Inventory{}, nil
}
func (routeCommerceService) ListInventory(context.Context, order.Actor, order.PageRequest) (order.InventoryPage, error) {
	return order.InventoryPage{}, nil
}
func (routeCommerceService) CreateOrder(context.Context, order.Actor, order.CreateOrderRequest) (order.Order, error) {
	return order.Order{}, nil
}
func (routeCommerceService) ListOrders(context.Context, order.Actor, order.PageRequest) (order.OrderPage, error) {
	return order.OrderPage{}, nil
}
func (routeCommerceService) GetOrder(context.Context, order.Actor, uuid.UUID) (order.Order, error) {
	return order.Order{}, nil
}

type routePaymentService struct{}

func (routePaymentService) CreatePayment(context.Context, payment.Actor, payment.CreateRequest) (payment.Payment, error) {
	return payment.Payment{}, nil
}
func (routePaymentService) GetPayment(context.Context, payment.Actor, uuid.UUID) (payment.Payment, error) {
	return payment.Payment{}, nil
}
