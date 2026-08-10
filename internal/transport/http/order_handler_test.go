package httptransport

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/assumeengagetry/distributed-commerce/internal/auth"
	commerce "github.com/assumeengagetry/distributed-commerce/internal/order"
)

func TestOrderRouterPublicProductsAndCustomerOrder(t *testing.T) {
	t.Parallel()
	productID, customerID := uuid.New(), uuid.New()
	now := transportTestNow
	var capturedActor commerce.Actor
	service := &stubCommerceService{
		listProducts: func(context.Context, commerce.PageRequest) (commerce.ProductPage, error) {
			return commerce.ProductPage{Products: []commerce.Product{{
				ID: productID, SKU: "VISIBLE-SKU", Name: "Visible Product", Description: "public",
				PriceAmount: 1500, Currency: commerce.CurrencyUSD, Status: commerce.ProductStatusActive,
				Version: 2, Available: true, CreatedAt: now, UpdatedAt: now,
			}}}, nil
		},
		createOrder: func(_ context.Context, actor commerce.Actor, request commerce.CreateOrderRequest) (commerce.Order, error) {
			capturedActor = actor
			return commerce.Order{
				ID: uuid.New(), UserID: actor.UserID, Status: commerce.OrderStatusPending,
				Currency: commerce.CurrencyUSD, TotalAmount: 3000, CreatedAt: now, UpdatedAt: now,
				Items: []commerce.OrderItem{{
					ID: uuid.New(), ProductID: request.Items[0].ProductID, ProductSKU: "VISIBLE-SKU",
					ProductName: "Visible Product", ProductVersion: 2,
					Quantity: 2, UnitPriceAmount: 1500, LineAmount: 3000,
				}},
			}, nil
		},
	}
	router := newOrderTestRouter(t, auth.Principal{UserID: customerID, Role: commerce.RoleCustomer, TokenID: uuid.New()}, service)

	products := performOrderRequest(router, http.MethodGet, "/v1/products?limit=20", "", "")
	if products.Code != http.StatusOK {
		t.Fatalf("products status = %d; body=%s", products.Code, products.Body.String())
	}
	if strings.Contains(products.Body.String(), `"quantity"`) || strings.Contains(products.Body.String(), `"status"`) {
		t.Fatalf("public product response exposed admin fields: %s", products.Body.String())
	}
	if !strings.Contains(products.Body.String(), `"availability":"in_stock"`) {
		t.Fatalf("public product response = %s", products.Body.String())
	}

	orderBody := `{"items":[{"product_id":"` + productID.String() + `","quantity":2,"expected_product_version":2}]}`
	created := performOrderRequest(router, http.MethodPost, "/v1/orders", orderBody, "token")
	if created.Code != http.StatusCreated {
		t.Fatalf("create order status = %d; body=%s", created.Code, created.Body.String())
	}
	if capturedActor.UserID != customerID || capturedActor.Role != commerce.RoleCustomer {
		t.Fatalf("captured actor = %+v", capturedActor)
	}
	if !strings.HasPrefix(created.Header().Get("Location"), "/v1/orders/") {
		t.Fatalf("Location = %q", created.Header().Get("Location"))
	}
	if decoded := decodeOrderResponse(t, created); decoded.TotalAmount != 3000 {
		t.Fatalf("created order total = %d, want 3000", decoded.TotalAmount)
	}
}

func TestOrderRouterAdminProductAndRoleAuthorization(t *testing.T) {
	t.Parallel()
	adminID, productID := uuid.New(), uuid.New()
	var createCalls int
	service := &stubCommerceService{createProduct: func(
		_ context.Context,
		actor commerce.Actor,
		request commerce.CreateProductRequest,
	) (commerce.AdminProduct, error) {
		createCalls++
		return commerce.AdminProduct{
			Product: commerce.Product{
				ID: productID, SKU: request.SKU, Name: request.Name, PriceAmount: request.PriceAmount,
				Currency: request.Currency, Status: request.Status, Version: 1, Available: request.InitialQuantity > 0,
			},
			Inventory: commerce.Inventory{
				ProductID: productID, SKU: request.SKU, Name: request.Name,
				Status: request.Status, Quantity: request.InitialQuantity, Version: 1,
			},
		}, nil
	}}
	adminRouter := newOrderTestRouter(t, auth.Principal{UserID: adminID, Role: commerce.RoleAdmin, TokenID: uuid.New()}, service)
	body := `{"sku":"ADMIN-SKU","name":"Admin Product","description":"","price_amount":999,"currency":"USD","status":"active","initial_quantity":5}`
	response := performOrderRequest(adminRouter, http.MethodPost, "/v1/admin/products", body, "token")
	if response.Code != http.StatusCreated || createCalls != 1 {
		t.Fatalf("admin create = status:%d calls:%d body:%s", response.Code, createCalls, response.Body.String())
	}
	if response.Header().Get("Location") != "/v1/admin/products/"+productID.String() {
		t.Fatalf("Location = %q", response.Header().Get("Location"))
	}

	customerRouter := newOrderTestRouter(t, auth.Principal{
		UserID: uuid.New(), Role: commerce.RoleCustomer, TokenID: uuid.New(),
	}, service)
	denied := performOrderRequest(customerRouter, http.MethodPost, "/v1/admin/products", body, "token")
	assertAPIError(t, denied, http.StatusForbidden, "FORBIDDEN")
	if createCalls != 1 {
		t.Fatalf("forbidden request called service; calls = %d", createCalls)
	}

	adminOrder := performOrderRequest(adminRouter, http.MethodPost, "/v1/orders", `{"items":[]}`, "token")
	assertAPIError(t, adminOrder, http.StatusForbidden, "FORBIDDEN")
}

func TestOrderRouterStrictJSONAndOwnedErrors(t *testing.T) {
	t.Parallel()
	customerID, productID := uuid.New(), uuid.New()
	service := &stubCommerceService{createOrder: func(
		context.Context,
		commerce.Actor,
		commerce.CreateOrderRequest,
	) (commerce.Order, error) {
		return commerce.Order{}, commerce.ErrInsufficientInventory
	}}
	router := newOrderTestRouter(t, auth.Principal{
		UserID: customerID, Role: commerce.RoleCustomer, TokenID: uuid.New(),
	}, service)

	duplicate := `{"items":[{"product_id":"` + productID.String() + `","quantity":1,"quantity":2,"expected_product_version":1}]}`
	response := performOrderRequest(router, http.MethodPost, "/v1/orders", duplicate, "token")
	assertAPIError(t, response, http.StatusBadRequest, "INVALID_REQUEST")
	caseAlias := `{"items":[{"product_id":"` + productID.String() + `","quantity":1,"Quantity":2,"expected_product_version":1}]}`
	response = performOrderRequest(router, http.MethodPost, "/v1/orders", caseAlias, "token")
	assertAPIError(t, response, http.StatusBadRequest, "INVALID_REQUEST")
	nullBody := performOrderRequest(router, http.MethodPost, "/v1/orders", `null`, "token")
	assertAPIError(t, nullBody, http.StatusBadRequest, "INVALID_REQUEST")

	valid := `{"items":[{"product_id":"` + productID.String() + `","quantity":1,"expected_product_version":1}]}`
	response = performOrderRequest(router, http.MethodPost, "/v1/orders", valid, "token")
	assertAPIError(t, response, http.StatusConflict, "INSUFFICIENT_INVENTORY")

	missing := performOrderRequest(router, http.MethodGet, "/does-not-exist", "", "")
	assertAPIError(t, missing, http.StatusNotFound, "ROUTE_NOT_FOUND")
	method := performOrderRequest(router, http.MethodPut, "/v1/products/"+productID.String(), "", "")
	assertAPIError(t, method, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED")
	malformedQuery := performOrderRequest(router, http.MethodGet, "/v1/products?cursor=value;limit=1", "", "")
	assertAPIError(t, malformedQuery, http.StatusBadRequest, "INVALID_QUERY")
}

func TestOrderRouterLimitsInvalidTokensBeforeVerification(t *testing.T) {
	t.Parallel()
	router, err := NewOrderRouter(OrderDependencies{
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)), ServiceName: "order-service",
		ReadinessCheck: func(context.Context) error { return nil }, ReadinessTimeout: time.Second,
		CommerceService: &stubCommerceService{},
		TokenVerifier:   stubTokenVerifier{err: auth.ErrInvalidAccessToken},
		RateLimit: RateLimitConfig{
			RequestsPerSecond: 0.1, Burst: 1, EntryTTL: time.Minute, MaxEntries: 100,
		},
		Now: func() time.Time { return transportTestNow },
	})
	if err != nil {
		t.Fatalf("NewOrderRouter() error = %v", err)
	}
	first := performOrderRequest(router, http.MethodGet, "/v1/orders", "", "invalid")
	assertAPIError(t, first, http.StatusUnauthorized, "UNAUTHORIZED")
	second := performOrderRequest(router, http.MethodGet, "/v1/orders", "", "invalid")
	assertAPIError(t, second, http.StatusTooManyRequests, "RATE_LIMITED")
}

func TestCommerceServiceErrorMappings(t *testing.T) {
	t.Parallel()
	productID := uuid.New()
	tests := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{name: "product changed", err: commerce.ErrProductChanged, status: http.StatusConflict, code: "PRODUCT_CHANGED"},
		{name: "amount", err: commerce.ErrOrderAmountTooLarge, status: http.StatusUnprocessableEntity, code: "ORDER_AMOUNT_TOO_LARGE"},
		{name: "temporary", err: commerce.ErrTemporarilyUnavailable, status: http.StatusServiceUnavailable, code: "SERVICE_UNAVAILABLE"},
		{name: "internal", err: errors.New("database details"), status: http.StatusInternalServerError, code: "INTERNAL_ERROR"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			service := &stubCommerceService{createOrder: func(
				context.Context,
				commerce.Actor,
				commerce.CreateOrderRequest,
			) (commerce.Order, error) {
				return commerce.Order{}, test.err
			}}
			router := newOrderTestRouter(t, auth.Principal{
				UserID: uuid.New(), Role: commerce.RoleCustomer, TokenID: uuid.New(),
			}, service)
			body := `{"items":[{"product_id":"` + productID.String() + `","quantity":1,"expected_product_version":1}]}`
			response := performOrderRequest(router, http.MethodPost, "/v1/orders", body, "token")
			assertAPIError(t, response, test.status, test.code)
			if strings.Contains(response.Body.String(), "database details") {
				t.Fatalf("response exposed internal error: %s", response.Body.String())
			}
		})
	}
}

func TestPaginationCursorRoundTripAndValidation(t *testing.T) {
	t.Parallel()
	cursor := &commerce.PageCursor{CreatedAt: transportTestNow, ID: uuid.New()}
	encoded, err := encodeCursor(cursor)
	if err != nil || encoded == nil {
		t.Fatalf("encodeCursor() = (%v, %v)", encoded, err)
	}
	decoded, err := decodeCursor(*encoded)
	if err != nil || !decoded.CreatedAt.Equal(cursor.CreatedAt) || decoded.ID != cursor.ID {
		t.Fatalf("decodeCursor() = (%+v, %v)", decoded, err)
	}
	unknown := base64.RawURLEncoding.EncodeToString([]byte(`{"v":1,"created_at":"2026-08-10T12:00:00Z","id":"` + cursor.ID.String() + `","extra":true}`))
	for _, invalid := range []string{"%%%", strings.Repeat("a", maximumCursorSize+1), unknown} {
		if _, err := decodeCursor(invalid); !errors.Is(err, errInvalidCursor) {
			t.Fatalf("decodeCursor(%q) error = %v, want errInvalidCursor", invalid, err)
		}
	}
}

func newOrderTestRouter(t *testing.T, principal auth.Principal, service CommerceService) http.Handler {
	t.Helper()
	router, err := NewOrderRouter(OrderDependencies{
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)), ServiceName: "order-service",
		ReadinessCheck: func(context.Context) error { return nil }, ReadinessTimeout: time.Second,
		CommerceService: service, TokenVerifier: stubTokenVerifier{principal: principal},
		RateLimit: RateLimitConfig{RequestsPerSecond: 1000, Burst: 100, EntryTTL: time.Minute, MaxEntries: 100},
		Now:       func() time.Time { return transportTestNow },
	})
	if err != nil {
		t.Fatalf("NewOrderRouter() error = %v", err)
	}
	return router
}

func performOrderRequest(handler http.Handler, method, path, body, token string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

type stubCommerceService struct {
	createProduct func(context.Context, commerce.Actor, commerce.CreateProductRequest) (commerce.AdminProduct, error)
	listProducts  func(context.Context, commerce.PageRequest) (commerce.ProductPage, error)
	createOrder   func(context.Context, commerce.Actor, commerce.CreateOrderRequest) (commerce.Order, error)
}

func (s *stubCommerceService) CreateProduct(ctx context.Context, actor commerce.Actor, request commerce.CreateProductRequest) (commerce.AdminProduct, error) {
	if s.createProduct == nil {
		return commerce.AdminProduct{}, nil
	}
	return s.createProduct(ctx, actor, request)
}

func (s *stubCommerceService) UpdateProduct(context.Context, commerce.Actor, uuid.UUID, commerce.UpdateProductRequest) (commerce.AdminProduct, error) {
	return commerce.AdminProduct{}, nil
}

func (s *stubCommerceService) GetProduct(context.Context, uuid.UUID) (commerce.Product, error) {
	return commerce.Product{}, nil
}

func (s *stubCommerceService) GetAdminProduct(context.Context, commerce.Actor, uuid.UUID) (commerce.AdminProduct, error) {
	return commerce.AdminProduct{}, nil
}

func (s *stubCommerceService) ListProducts(ctx context.Context, page commerce.PageRequest) (commerce.ProductPage, error) {
	if s.listProducts == nil {
		return commerce.ProductPage{}, nil
	}
	return s.listProducts(ctx, page)
}

func (s *stubCommerceService) AdjustInventory(context.Context, commerce.Actor, uuid.UUID, commerce.AdjustInventoryRequest) (commerce.Inventory, error) {
	return commerce.Inventory{}, nil
}

func (s *stubCommerceService) ListInventory(context.Context, commerce.Actor, commerce.PageRequest) (commerce.InventoryPage, error) {
	return commerce.InventoryPage{}, nil
}

func (s *stubCommerceService) CreateOrder(ctx context.Context, actor commerce.Actor, request commerce.CreateOrderRequest) (commerce.Order, error) {
	if s.createOrder == nil {
		return commerce.Order{}, nil
	}
	return s.createOrder(ctx, actor, request)
}

func (s *stubCommerceService) ListOrders(context.Context, commerce.Actor, commerce.PageRequest) (commerce.OrderPage, error) {
	return commerce.OrderPage{}, nil
}

func (s *stubCommerceService) GetOrder(context.Context, commerce.Actor, uuid.UUID) (commerce.Order, error) {
	return commerce.Order{}, nil
}

func decodeOrderResponse(t *testing.T, response *httptest.ResponseRecorder) orderResponse {
	t.Helper()
	var envelope struct {
		Order orderResponse `json:"order"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode order response: %v", err)
	}
	return envelope.Order
}
