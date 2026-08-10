//go:build integration

package httptransport

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/assumeengagetry/distributed-commerce/internal/auth"
	"github.com/assumeengagetry/distributed-commerce/internal/config"
	"github.com/assumeengagetry/distributed-commerce/internal/database"
	store "github.com/assumeengagetry/distributed-commerce/internal/database/sqlc"
	commerce "github.com/assumeengagetry/distributed-commerce/internal/order"
)

func TestCommerceAPIIntegration(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, err := database.Open(ctx, config.DatabaseConfig{
		URL: databaseURL, MaxConns: 20, MinIdleConns: 1,
		MaxConnLifetime: time.Hour, MaxConnLifetimeJitter: 5 * time.Minute,
		MaxConnIdleTime: 30 * time.Minute, HealthCheckPeriod: time.Minute, PingTimeout: 3 * time.Second,
		OperationTimeout: 5 * time.Second, LockTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	t.Cleanup(pool.Close)

	adminID := createTransportCommerceActor(t, ctx, pool, commerce.RoleAdmin)
	customerID := createTransportCommerceActor(t, ctx, pool, commerce.RoleCustomer)
	otherCustomerID := createTransportCommerceActor(t, ctx, pool, commerce.RoleCustomer)
	actorIDs := []uuid.UUID{adminID, customerID, otherCustomerID}
	var productID uuid.UUID
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		if _, err := pool.Exec(cleanupCtx, `DELETE FROM orders WHERE user_id = ANY($1::uuid[])`, actorIDs); err != nil {
			t.Errorf("delete commerce API orders: %v", err)
		}
		if productID != uuid.Nil {
			if _, err := pool.Exec(cleanupCtx, `DELETE FROM inventories WHERE product_id = $1`, productID); err != nil {
				t.Errorf("delete commerce API inventory: %v", err)
			}
			if _, err := pool.Exec(cleanupCtx, `DELETE FROM products WHERE id = $1`, productID); err != nil {
				t.Errorf("delete commerce API product: %v", err)
			}
		}
		if _, err := pool.Exec(cleanupCtx, `DELETE FROM users WHERE id = ANY($1::uuid[])`, actorIDs); err != nil {
			t.Errorf("delete commerce API actors: %v", err)
		}
	})

	tokens, err := auth.NewTokenManager(integrationTokenKey, "integration-user-service", 15*time.Minute, 30*time.Second)
	if err != nil {
		t.Fatalf("NewTokenManager() error = %v", err)
	}
	adminToken := issueIntegrationAccess(t, tokens, adminID, commerce.RoleAdmin)
	customerToken := issueIntegrationAccess(t, tokens, customerID, commerce.RoleCustomer)
	otherCustomerToken := issueIntegrationAccess(t, tokens, otherCustomerID, commerce.RoleCustomer)
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	service, err := commerce.NewService(
		database.NewOrderRepository(pool, 2*time.Second, 5*time.Second), logger, 5*time.Second,
	)
	if err != nil {
		t.Fatalf("commerce.NewService() error = %v", err)
	}
	queries := store.New(pool)
	router, err := NewOrderRouter(OrderDependencies{
		Logger: logger, ServiceName: "order-service",
		ReadinessCheck: func(ctx context.Context) error {
			value, err := queries.OrderHealthCheck(ctx)
			if err == nil && value != 1 {
				return fmt.Errorf("order schema is not ready")
			}
			return err
		},
		ReadinessTimeout: 3 * time.Second, CommerceService: service, TokenVerifier: tokens,
		RateLimit: RateLimitConfig{RequestsPerSecond: 1000, Burst: 100, EntryTTL: time.Minute, MaxEntries: 100},
		Now:       time.Now,
	})
	if err != nil {
		t.Fatalf("NewOrderRouter() error = %v", err)
	}

	ready := performJSONRequest(t, router, http.MethodGet, "/readyz", nil, "")
	if ready.Code != http.StatusOK {
		t.Fatalf("ready status = %d; body=%s", ready.Code, ready.Body.String())
	}
	createProduct := performJSONRequest(t, router, http.MethodPost, "/v1/admin/products", map[string]any{
		"sku": "API-" + strings.ToUpper(uuid.NewString()[:8]), "name": "API Phase 3 Product", "description": "snapshot test",
		"price_amount": 1500, "currency": "USD", "status": "active", "initial_quantity": 2,
	}, adminToken)
	if createProduct.Code != http.StatusCreated {
		t.Fatalf("create product status = %d; body=%s", createProduct.Code, createProduct.Body.String())
	}
	locationID := strings.TrimPrefix(createProduct.Header().Get("Location"), "/v1/admin/products/")
	productID, err = uuid.Parse(locationID)
	if err != nil || productID == uuid.Nil {
		t.Fatalf("create product Location = %q", createProduct.Header().Get("Location"))
	}
	var productEnvelope adminProductResponse
	if err := json.Unmarshal(createProduct.Body.Bytes(), &productEnvelope); err != nil {
		t.Fatalf("decode product response: %v", err)
	}
	if productEnvelope.Product.ID != productID || productEnvelope.Inventory.Quantity != 2 || productEnvelope.Product.Version != 1 {
		t.Fatalf("created product response = %+v", productEnvelope)
	}

	publicProduct := performJSONRequest(t, router, http.MethodGet, "/v1/products/"+productID.String(), nil, "")
	if publicProduct.Code != http.StatusOK {
		t.Fatalf("public product status = %d; body=%s", publicProduct.Code, publicProduct.Body.String())
	}
	if containsJSONKey(publicProduct.Body.Bytes(), "quantity") {
		t.Fatalf("public product exposed inventory quantity: %s", publicProduct.Body.String())
	}

	deniedAdmin := performJSONRequest(t, router, http.MethodPatch, "/v1/admin/inventory/"+productID.String(), map[string]any{
		"expected_version": 1, "delta": 1,
	}, customerToken)
	assertAPIError(t, deniedAdmin, http.StatusForbidden, "FORBIDDEN")

	createOrder := performJSONRequest(t, router, http.MethodPost, "/v1/orders", map[string]any{
		"items": []map[string]any{{
			"product_id": productID, "quantity": 2, "expected_product_version": 1,
		}},
	}, customerToken)
	if createOrder.Code != http.StatusCreated {
		t.Fatalf("create order status = %d; body=%s", createOrder.Code, createOrder.Body.String())
	}
	createdOrder := decodeOrderResponse(t, createOrder)
	if createdOrder.TotalAmount != 3000 || len(createdOrder.Items) != 1 || createdOrder.Items[0].UnitPriceAmount != 1500 {
		t.Fatalf("created order = %+v", createdOrder)
	}

	insufficient := performJSONRequest(t, router, http.MethodPost, "/v1/orders", map[string]any{
		"items": []map[string]any{{
			"product_id": productID, "quantity": 1, "expected_product_version": 1,
		}},
	}, customerToken)
	assertAPIError(t, insufficient, http.StatusConflict, "INSUFFICIENT_INVENTORY")

	newPrice := int64(2000)
	updateProduct := performJSONRequest(t, router, http.MethodPatch, "/v1/admin/products/"+productID.String(), map[string]any{
		"expected_version": 1, "price_amount": newPrice,
	}, adminToken)
	if updateProduct.Code != http.StatusOK {
		t.Fatalf("update product status = %d; body=%s", updateProduct.Code, updateProduct.Body.String())
	}

	history := performJSONRequest(t, router, http.MethodGet, "/v1/orders/"+createdOrder.ID.String(), nil, customerToken)
	if history.Code != http.StatusOK {
		t.Fatalf("order history status = %d; body=%s", history.Code, history.Body.String())
	}
	historicOrder := decodeOrderResponse(t, history)
	if historicOrder.Items[0].UnitPriceAmount != 1500 || historicOrder.Items[0].ProductVersion != 1 {
		t.Fatalf("historic snapshot changed = %+v", historicOrder.Items[0])
	}
	crossCustomer := performJSONRequest(t, router, http.MethodGet, "/v1/orders/"+createdOrder.ID.String(), nil, otherCustomerToken)
	assertAPIError(t, crossCustomer, http.StatusNotFound, "ORDER_NOT_FOUND")
	adminHistory := performJSONRequest(t, router, http.MethodGet, "/v1/orders?limit=20", nil, adminToken)
	if adminHistory.Code != http.StatusOK || !containsJSONUUID(adminHistory.Body.Bytes(), createdOrder.ID) {
		t.Fatalf("admin history status = %d; body=%s", adminHistory.Code, adminHistory.Body.String())
	}

	restock := performJSONRequest(t, router, http.MethodPatch, "/v1/admin/inventory/"+productID.String(), map[string]any{
		"expected_version": 2, "delta": 3,
	}, adminToken)
	if restock.Code != http.StatusOK {
		t.Fatalf("restock status = %d; body=%s", restock.Code, restock.Body.String())
	}
	var inventoryEnvelope struct {
		Inventory inventoryResponse `json:"inventory"`
	}
	if err := json.Unmarshal(restock.Body.Bytes(), &inventoryEnvelope); err != nil {
		t.Fatalf("decode inventory response: %v", err)
	}
	if inventoryEnvelope.Inventory.Quantity != 3 || inventoryEnvelope.Inventory.Version != 3 {
		t.Fatalf("restocked inventory = %+v", inventoryEnvelope.Inventory)
	}

	if _, err := pool.Exec(ctx, `UPDATE users SET role = 'customer', updated_at = now() WHERE id = $1`, adminID); err != nil {
		t.Fatalf("demote admin: %v", err)
	}
	staleAdminToken := performJSONRequest(t, router, http.MethodPatch, "/v1/admin/inventory/"+productID.String(), map[string]any{
		"expected_version": 3, "delta": 1,
	}, adminToken)
	assertAPIError(t, staleAdminToken, http.StatusForbidden, "FORBIDDEN")
	staleAdminRead := performJSONRequest(t, router, http.MethodGet, "/v1/admin/inventory", nil, adminToken)
	assertAPIError(t, staleAdminRead, http.StatusForbidden, "FORBIDDEN")

	if _, err := pool.Exec(ctx, `UPDATE users SET status = 'disabled', updated_at = now() WHERE id = $1`, customerID); err != nil {
		t.Fatalf("disable customer: %v", err)
	}
	disabledCustomer := performJSONRequest(t, router, http.MethodGet, "/v1/orders", nil, customerToken)
	assertAPIError(t, disabledCustomer, http.StatusUnauthorized, "ACCOUNT_DISABLED")
	if disabledCustomer.Header().Get("WWW-Authenticate") != "Bearer" {
		t.Fatalf("disabled customer challenge = %q", disabledCustomer.Header().Get("WWW-Authenticate"))
	}
}

func createTransportCommerceActor(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	role string,
) uuid.UUID {
	t.Helper()
	userID, accountID := uuid.New(), uuid.New()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin actor transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `
		INSERT INTO users (id, email, password_hash, display_name, role, status)
		VALUES ($1, $2, '$argon2id$integration-test', 'Transport Commerce', $3, 'active')
	`, userID, fmt.Sprintf("transport-commerce-%s@example.com", uuid.NewString()), role); err != nil {
		t.Fatalf("insert transport actor: %v", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO accounts (id, user_id, currency) VALUES ($1, $2, 'USD')`, accountID, userID); err != nil {
		t.Fatalf("insert transport account: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit transport actor: %v", err)
	}
	return userID
}

func issueIntegrationAccess(t *testing.T, tokens *auth.TokenManager, userID uuid.UUID, role string) string {
	t.Helper()
	encoded, _, err := tokens.IssueAccess(userID, role, time.Now().UTC())
	if err != nil {
		t.Fatalf("IssueAccess() error = %v", err)
	}
	return encoded
}

func containsJSONKey(data []byte, key string) bool {
	var decoded any
	if json.Unmarshal(data, &decoded) != nil {
		return false
	}
	return jsonContainsKey(decoded, key)
}

func jsonContainsKey(value any, key string) bool {
	switch typed := value.(type) {
	case map[string]any:
		for candidate, nested := range typed {
			if candidate == key || jsonContainsKey(nested, key) {
				return true
			}
		}
	case []any:
		for _, nested := range typed {
			if jsonContainsKey(nested, key) {
				return true
			}
		}
	}
	return false
}

func containsJSONUUID(data []byte, id uuid.UUID) bool {
	return containsJSONValue(data, id.String())
}

func containsJSONValue(data []byte, expected string) bool {
	var decoded any
	if json.Unmarshal(data, &decoded) != nil {
		return false
	}
	var visit func(any) bool
	visit = func(value any) bool {
		switch typed := value.(type) {
		case string:
			return typed == expected
		case map[string]any:
			for _, nested := range typed {
				if visit(nested) {
					return true
				}
			}
		case []any:
			for _, nested := range typed {
				if visit(nested) {
					return true
				}
			}
		}
		return false
	}
	return visit(decoded)
}
