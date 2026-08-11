//go:build integration

package httptransport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/test/bufconn"

	"github.com/assumeengagetry/distributed-commerce/internal/auth"
	"github.com/assumeengagetry/distributed-commerce/internal/config"
	"github.com/assumeengagetry/distributed-commerce/internal/database"
	store "github.com/assumeengagetry/distributed-commerce/internal/database/sqlc"
	identityv1 "github.com/assumeengagetry/distributed-commerce/internal/genproto/identity/v1"
	"github.com/assumeengagetry/distributed-commerce/internal/identity"
	commerce "github.com/assumeengagetry/distributed-commerce/internal/order"
	"github.com/assumeengagetry/distributed-commerce/internal/payment"
	grpctransport "github.com/assumeengagetry/distributed-commerce/internal/transport/grpc"
)

func TestPaymentAPIIntegration(t *testing.T) {
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
		OperationTimeout: 5 * time.Second, LockTimeout: 2 * time.Second, CommitResolutionTimeout: 2 * time.Second,
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
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM payments WHERE user_id = ANY($1::uuid[])`, actorIDs)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM orders WHERE user_id = ANY($1::uuid[])`, actorIDs)
		if productID != uuid.Nil {
			_, _ = pool.Exec(cleanupCtx, `DELETE FROM inventories WHERE product_id = $1`, productID)
			_, _ = pool.Exec(cleanupCtx, `DELETE FROM products WHERE id = $1`, productID)
		}
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM users WHERE id = ANY($1::uuid[])`, actorIDs)
	})
	if _, err := pool.Exec(ctx, `UPDATE accounts SET balance = 5000, updated_at = now() WHERE user_id = $1`, customerID); err != nil {
		t.Fatalf("fund payment API account: %v", err)
	}

	tokens, err := auth.NewTokenManager(integrationTokenKey, "integration-user-service", 15*time.Minute, 30*time.Second)
	if err != nil {
		t.Fatalf("NewTokenManager() error = %v", err)
	}
	adminToken := issueIntegrationAccess(t, tokens, adminID, commerce.RoleAdmin)
	customerToken := issueIntegrationAccess(t, tokens, customerID, commerce.RoleCustomer)
	otherCustomerToken := issueIntegrationAccess(t, tokens, otherCustomerID, commerce.RoleCustomer)
	identityClient := startIntegrationIdentityClient(t, tokens)
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	queries := store.New(pool)
	orderService, err := commerce.NewService(
		database.NewOrderRepository(pool, 2*time.Second, 5*time.Second, 2*time.Second), logger, 5*time.Second,
	)
	if err != nil {
		t.Fatalf("commerce.NewService() error = %v", err)
	}
	orderRouter, err := NewOrderRouter(OrderDependencies{
		Logger: logger, ServiceName: "order-service",
		ReadinessChecks: map[string]func(context.Context) error{"postgres": func(ctx context.Context) error {
			value, err := queries.OrderHealthCheck(ctx)
			if err == nil && value != 1 {
				return fmt.Errorf("order schema is not ready")
			}
			return err
		}, "identity_grpc": identityClient.Health},
		ReadinessTimeout: 3 * time.Second, CommerceService: orderService, TokenVerifier: identityClient,
		RateLimit: RateLimitConfig{RequestsPerSecond: 1000, Burst: 100, EntryTTL: time.Minute, MaxEntries: 100},
		Now:       time.Now,
	})
	if err != nil {
		t.Fatalf("NewOrderRouter() error = %v", err)
	}
	paymentService, err := payment.NewService(
		database.NewPaymentRepository(pool, 2*time.Second, 5*time.Second, 2*time.Second), logger, 5*time.Second,
	)
	if err != nil {
		t.Fatalf("payment.NewService() error = %v", err)
	}
	paymentRouter, err := NewPaymentRouter(PaymentDependencies{
		Logger: logger, ServiceName: "payment-service",
		ReadinessChecks: map[string]func(context.Context) error{"postgres": func(ctx context.Context) error {
			value, err := queries.PaymentHealthCheck(ctx)
			if err == nil && value != 1 {
				return fmt.Errorf("payment schema is not ready")
			}
			return err
		}, "identity_grpc": identityClient.Health},
		ReadinessTimeout: 3 * time.Second, PaymentService: paymentService, TokenVerifier: identityClient,
		RateLimit: RateLimitConfig{RequestsPerSecond: 1000, Burst: 100, EntryTTL: time.Minute, MaxEntries: 100},
		Now:       time.Now,
	})
	if err != nil {
		t.Fatalf("NewPaymentRouter() error = %v", err)
	}
	ready := performJSONRequest(t, paymentRouter, http.MethodGet, "/readyz", nil, "")
	if ready.Code != http.StatusOK {
		t.Fatalf("payment ready status = %d; body=%s", ready.Code, ready.Body.String())
	}

	createdProduct := performJSONRequest(t, orderRouter, http.MethodPost, "/v1/admin/products", map[string]any{
		"sku": "PAYAPI-" + strings.ToUpper(uuid.NewString()[:8]), "name": "Payment API Product",
		"description": "payment integration", "price_amount": 1200,
		"currency": "USD", "status": "active", "initial_quantity": 2,
	}, adminToken)
	if createdProduct.Code != http.StatusCreated {
		t.Fatalf("create payment product status = %d; body=%s", createdProduct.Code, createdProduct.Body.String())
	}
	var productEnvelope adminProductResponse
	if err := json.Unmarshal(createdProduct.Body.Bytes(), &productEnvelope); err != nil {
		t.Fatalf("decode payment product: %v", err)
	}
	productID = productEnvelope.Product.ID
	orderBody := map[string]any{
		"items": []map[string]any{{
			"product_id": productID, "quantity": 1, "expected_product_version": 1,
		}},
	}
	createdOrder := performJSONRequestWithIdempotency(
		t, orderRouter, http.MethodPost, "/v1/orders", orderBody, customerToken, "payment-api-shared-key",
	)
	if createdOrder.Code != http.StatusCreated {
		t.Fatalf("create payment order status = %d; body=%s", createdOrder.Code, createdOrder.Body.String())
	}
	order := decodeOrderResponse(t, createdOrder)
	if order.Status != commerce.OrderStatusPending {
		t.Fatalf("created order status = %q", order.Status)
	}

	paymentBody := map[string]any{"order_id": order.ID}
	createdPayment := performJSONRequestWithIdempotency(
		t, paymentRouter, http.MethodPost, "/v1/payments", paymentBody, customerToken, "payment-api-shared-key",
	)
	if createdPayment.Code != http.StatusCreated {
		t.Fatalf("create payment status = %d; body=%s", createdPayment.Code, createdPayment.Body.String())
	}
	var paymentEnvelope struct {
		Payment paymentResponse `json:"payment"`
	}
	if err := json.Unmarshal(createdPayment.Body.Bytes(), &paymentEnvelope); err != nil {
		t.Fatalf("decode payment API response: %v", err)
	}
	if paymentEnvelope.Payment.Amount != 1200 || paymentEnvelope.Payment.Status != payment.StatusSucceeded {
		t.Fatalf("created payment = %+v", paymentEnvelope.Payment)
	}
	replayedPayment := performJSONRequestWithIdempotency(
		t, paymentRouter, http.MethodPost, "/v1/payments", paymentBody, customerToken, "payment-api-shared-key",
	)
	if replayedPayment.Code != http.StatusCreated || replayedPayment.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("replay status = %d headers=%v body=%s", replayedPayment.Code, replayedPayment.Header(), replayedPayment.Body.String())
	}
	if !bytes.Equal(createdPayment.Body.Bytes(), replayedPayment.Body.Bytes()) {
		t.Fatalf("payment replay body differs:\nfirst=%s\nreplay=%s", createdPayment.Body.String(), replayedPayment.Body.String())
	}

	customerRead := performJSONRequest(
		t, paymentRouter, http.MethodGet, "/v1/payments/"+paymentEnvelope.Payment.ID.String(), nil, customerToken,
	)
	if customerRead.Code != http.StatusOK {
		t.Fatalf("customer payment read status = %d; body=%s", customerRead.Code, customerRead.Body.String())
	}
	adminRead := performJSONRequest(
		t, paymentRouter, http.MethodGet, "/v1/payments/"+paymentEnvelope.Payment.ID.String(), nil, adminToken,
	)
	if adminRead.Code != http.StatusOK {
		t.Fatalf("admin payment read status = %d; body=%s", adminRead.Code, adminRead.Body.String())
	}
	otherRead := performJSONRequest(
		t, paymentRouter, http.MethodGet, "/v1/payments/"+paymentEnvelope.Payment.ID.String(), nil, otherCustomerToken,
	)
	assertAPIError(t, otherRead, http.StatusNotFound, "PAYMENT_NOT_FOUND")
	paidOrder := performJSONRequest(t, orderRouter, http.MethodGet, "/v1/orders/"+order.ID.String(), nil, customerToken)
	if paidOrder.Code != http.StatusOK || decodeOrderResponse(t, paidOrder).Status != commerce.OrderStatusPaid {
		t.Fatalf("paid order response = status:%d body:%s", paidOrder.Code, paidOrder.Body.String())
	}
	replayedOrder := performJSONRequestWithIdempotency(
		t, orderRouter, http.MethodPost, "/v1/orders", orderBody, customerToken, "payment-api-shared-key",
	)
	if replayedOrder.Code != http.StatusCreated || replayedOrder.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("order replay status = %d headers=%v body=%s", replayedOrder.Code, replayedOrder.Header(), replayedOrder.Body.String())
	}
	if !bytes.Equal(createdOrder.Body.Bytes(), replayedOrder.Body.Bytes()) {
		t.Fatalf("order replay changed after payment:\nfirst=%s\nreplay=%s", createdOrder.Body.String(), replayedOrder.Body.String())
	}
	var balance int64
	if err := pool.QueryRow(ctx, `SELECT balance FROM accounts WHERE user_id = $1`, customerID).Scan(&balance); err != nil {
		t.Fatalf("query payment API balance: %v", err)
	}
	if balance != 3800 {
		t.Fatalf("payment API balance = %d, want 3800", balance)
	}
}

func startIntegrationIdentityClient(t *testing.T, tokens *auth.TokenManager) *identity.Client {
	t.Helper()
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	identityServer, err := grpctransport.NewIdentityServer(tokens, time.Now)
	if err != nil {
		t.Fatalf("NewIdentityServer() error = %v", err)
	}
	identityv1.RegisterIdentityServiceServer(server, identityServer)
	healthServer := health.NewServer()
	healthpb.RegisterHealthServer(server, healthServer)
	healthServer.SetServingStatus("identity.v1.IdentityService", healthpb.HealthCheckResponse_SERVING)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
	})
	connection, err := grpc.NewClient(
		"passthrough:///identity-integration",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
		grpc.WithDisableRetry(),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient() error = %v", err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	client, err := identity.NewClient(connection, time.Second)
	if err != nil {
		t.Fatalf("identity.NewClient() error = %v", err)
	}
	return client
}
