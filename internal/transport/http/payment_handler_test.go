package httptransport

import (
	"bytes"
	"context"
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
	"github.com/assumeengagetry/distributed-commerce/internal/payment"
)

func TestPaymentRouterCreateAndReplayResponse(t *testing.T) {
	t.Parallel()
	customerID, orderID, paymentID := uuid.New(), uuid.New(), uuid.New()
	var captured payment.CreateRequest
	service := &stubPaymentService{create: func(
		_ context.Context,
		actor payment.Actor,
		request payment.CreateRequest,
	) (payment.Payment, error) {
		captured = request
		return payment.Payment{
			ID: paymentID, OrderID: request.OrderID, UserID: actor.UserID,
			AccountID: uuid.New(), Status: payment.StatusSucceeded, Currency: "USD", Amount: 2500,
			BalanceBefore: 5000, BalanceAfter: 2500, CreatedAt: transportTestNow,
			IdempotencyReplay: true,
		}, nil
	}}
	router := newPaymentTestRouter(t, auth.Principal{
		UserID: customerID, Role: payment.RoleCustomer, TokenID: uuid.New(),
	}, service)
	response := performPaymentRequest(
		router, http.MethodPost, "/v1/payments",
		`{"order_id":"`+orderID.String()+`"}`, "token", "payment-handler-key",
	)
	if response.Code != http.StatusCreated {
		t.Fatalf("payment status = %d; body=%s", response.Code, response.Body.String())
	}
	if captured.OrderID != orderID || captured.IdempotencyKey != "payment-handler-key" {
		t.Fatalf("captured payment request = %+v", captured)
	}
	if response.Header().Get("Location") != "/v1/payments/"+paymentID.String() ||
		response.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("payment headers = %+v", response.Header())
	}
	if strings.Contains(response.Body.String(), "account_id") || strings.Contains(response.Body.String(), "balance_") {
		t.Fatalf("payment response exposed internal account data: %s", response.Body.String())
	}
}

func TestPaymentRouterRequiresOneValidIdempotencyHeader(t *testing.T) {
	t.Parallel()
	orderID := uuid.New()
	router := newPaymentTestRouter(t, auth.Principal{
		UserID: uuid.New(), Role: payment.RoleCustomer, TokenID: uuid.New(),
	}, &stubPaymentService{})
	body := `{"order_id":"` + orderID.String() + `"}`
	missing := performPaymentRequest(router, http.MethodPost, "/v1/payments", body, "token", "")
	assertAPIError(t, missing, http.StatusBadRequest, "IDEMPOTENCY_KEY_REQUIRED")

	request := httptest.NewRequest(http.MethodPost, "/v1/payments", bytes.NewBufferString(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer token")
	request.Header.Add("Idempotency-Key", "first-key")
	request.Header.Add("Idempotency-Key", "second-key")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	assertAPIError(t, response, http.StatusBadRequest, "INVALID_IDEMPOTENCY_KEY")
}

func TestPaymentRouterAuthorizationAndErrors(t *testing.T) {
	t.Parallel()
	orderID := uuid.New()
	adminRouter := newPaymentTestRouter(t, auth.Principal{
		UserID: uuid.New(), Role: payment.RoleAdmin, TokenID: uuid.New(),
	}, &stubPaymentService{})
	denied := performPaymentRequest(
		adminRouter, http.MethodPost, "/v1/payments",
		`{"order_id":"`+orderID.String()+`"}`, "token", "admin-payment-key",
	)
	assertAPIError(t, denied, http.StatusForbidden, "FORBIDDEN")

	tests := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{name: "funds", err: payment.ErrInsufficientFunds, status: http.StatusConflict, code: "INSUFFICIENT_FUNDS"},
		{name: "paid", err: payment.ErrOrderAlreadyPaid, status: http.StatusConflict, code: "ORDER_ALREADY_PAID"},
		{name: "idempotency", err: payment.ErrIdempotencyConflict, status: http.StatusConflict, code: "IDEMPOTENCY_KEY_REUSED"},
		{name: "outcome", err: payment.ErrOperationOutcomeUnknown, status: http.StatusServiceUnavailable, code: "OPERATION_OUTCOME_UNKNOWN"},
		{name: "internal", err: errors.New("private database detail"), status: http.StatusInternalServerError, code: "INTERNAL_ERROR"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			service := &stubPaymentService{create: func(
				context.Context, payment.Actor, payment.CreateRequest,
			) (payment.Payment, error) {
				return payment.Payment{}, test.err
			}}
			router := newPaymentTestRouter(t, auth.Principal{
				UserID: uuid.New(), Role: payment.RoleCustomer, TokenID: uuid.New(),
			}, service)
			response := performPaymentRequest(
				router, http.MethodPost, "/v1/payments",
				`{"order_id":"`+orderID.String()+`"}`, "token", "error-payment-key",
			)
			assertAPIError(t, response, test.status, test.code)
			if strings.Contains(response.Body.String(), "private database detail") {
				t.Fatalf("payment response exposed internal error: %s", response.Body.String())
			}
		})
	}
}

func newPaymentTestRouter(t *testing.T, principal auth.Principal, service PaymentService) http.Handler {
	t.Helper()
	router, err := NewPaymentRouter(PaymentDependencies{
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)), ServiceName: "payment-service",
		ReadinessChecks: map[string]func(context.Context) error{"postgres": func(context.Context) error { return nil }}, ReadinessTimeout: time.Second,
		PaymentService: service, TokenVerifier: stubTokenVerifier{principal: principal},
		RateLimit: RateLimitConfig{RequestsPerSecond: 1000, Burst: 100, EntryTTL: time.Minute, MaxEntries: 100},
		Now:       func() time.Time { return transportTestNow },
	})
	if err != nil {
		t.Fatalf("NewPaymentRouter() error = %v", err)
	}
	return router
}

func performPaymentRequest(
	handler http.Handler,
	method, path, body, token, idempotencyKey string,
) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if idempotencyKey != "" {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

type stubPaymentService struct {
	create func(context.Context, payment.Actor, payment.CreateRequest) (payment.Payment, error)
	get    func(context.Context, payment.Actor, uuid.UUID) (payment.Payment, error)
}

func (s *stubPaymentService) CreatePayment(
	ctx context.Context,
	actor payment.Actor,
	request payment.CreateRequest,
) (payment.Payment, error) {
	if s.create == nil {
		return payment.Payment{}, nil
	}
	return s.create(ctx, actor, request)
}

func (s *stubPaymentService) GetPayment(
	ctx context.Context,
	actor payment.Actor,
	paymentID uuid.UUID,
) (payment.Payment, error) {
	if s.get == nil {
		return payment.Payment{}, nil
	}
	return s.get(ctx, actor, paymentID)
}
