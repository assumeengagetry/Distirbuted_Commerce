package payment

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestServiceCreatePaymentBuildsStableIdempotencyHashes(t *testing.T) {
	t.Parallel()
	var captured []CreateParams
	repository := &stubRepository{createPayment: func(
		_ context.Context,
		actorID uuid.UUID,
		params CreateParams,
	) (Payment, error) {
		captured = append(captured, params)
		return Payment{ID: params.PaymentID, OrderID: params.OrderID, UserID: actorID}, nil
	}}
	service := newPaymentTestService(t, repository)
	actor := Actor{UserID: uuid.New(), Role: RoleCustomer}
	orderID := uuid.New()
	for range 2 {
		created, err := service.CreatePayment(t.Context(), actor, CreateRequest{
			OrderID: orderID, IdempotencyKey: "stable-payment-key",
		})
		if err != nil {
			t.Fatalf("CreatePayment() error = %v", err)
		}
		if created.ID == uuid.Nil || created.OrderID != orderID {
			t.Fatalf("created payment = %+v", created)
		}
	}
	if len(captured) != 2 || captured[0].PaymentID == captured[1].PaymentID {
		t.Fatalf("captured payment IDs = %+v", captured)
	}
	if !bytes.Equal(captured[0].KeyHash, captured[1].KeyHash) ||
		!bytes.Equal(captured[0].RequestHash, captured[1].RequestHash) {
		t.Fatalf("stable request produced different hashes: %+v", captured)
	}
	if len(captured[0].KeyHash) != 32 || len(captured[0].RequestHash) != 32 {
		t.Fatalf("hash lengths = %d/%d", len(captured[0].KeyHash), len(captured[0].RequestHash))
	}
}

func TestServiceCreatePaymentValidation(t *testing.T) {
	t.Parallel()
	service := newPaymentTestService(t, &stubRepository{})
	customer := Actor{UserID: uuid.New(), Role: RoleCustomer}
	orderID := uuid.New()
	tests := []struct {
		name    string
		actor   Actor
		request CreateRequest
		want    error
	}{
		{name: "wrong role", actor: Actor{UserID: uuid.New(), Role: RoleAdmin}, request: CreateRequest{OrderID: orderID, IdempotencyKey: "valid-key"}, want: ErrForbidden},
		{name: "order ID", actor: customer, request: CreateRequest{IdempotencyKey: "valid-key"}, want: ErrInvalidOrderID},
		{name: "missing key", actor: customer, request: CreateRequest{OrderID: orderID}, want: ErrIdempotencyKeyRequired},
		{name: "invalid key", actor: customer, request: CreateRequest{OrderID: orderID, IdempotencyKey: "invalid key"}, want: ErrInvalidIdempotencyKey},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := service.CreatePayment(t.Context(), test.actor, test.request); !errors.Is(err, test.want) {
				t.Fatalf("CreatePayment() error = %v, want %v", err, test.want)
			}
		})
	}
}

func newPaymentTestService(t *testing.T, repository Repository) *Service {
	t.Helper()
	service, err := NewService(
		repository, slog.New(slog.NewJSONHandler(io.Discard, nil)), 5*time.Second,
	)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	return service
}

type stubRepository struct {
	createPayment func(context.Context, uuid.UUID, CreateParams) (Payment, error)
	getPayment    func(context.Context, Actor, uuid.UUID) (Payment, error)
}

func (s *stubRepository) CreatePayment(ctx context.Context, actorID uuid.UUID, params CreateParams) (Payment, error) {
	if s.createPayment == nil {
		return Payment{}, nil
	}
	return s.createPayment(ctx, actorID, params)
}

func (s *stubRepository) GetPayment(ctx context.Context, actor Actor, paymentID uuid.UUID) (Payment, error) {
	if s.getPayment == nil {
		return Payment{}, nil
	}
	return s.getPayment(ctx, actor, paymentID)
}
