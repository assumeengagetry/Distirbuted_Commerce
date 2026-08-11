package payment

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/assumeengagetry/distributed-commerce/internal/idempotency"
)

type Service struct {
	repository Repository
	logger     *slog.Logger
	timeout    time.Duration
}

func NewService(repository Repository, logger *slog.Logger, timeout time.Duration) (*Service, error) {
	if repository == nil {
		return nil, fmt.Errorf("payment repository is required")
	}
	if logger == nil {
		return nil, fmt.Errorf("logger is required")
	}
	if timeout <= 0 {
		return nil, fmt.Errorf("database timeout must be positive")
	}
	return &Service{repository: repository, logger: logger, timeout: timeout}, nil
}

func (s *Service) CreatePayment(ctx context.Context, actor Actor, request CreateRequest) (Payment, error) {
	if err := validateActor(actor, RoleCustomer); err != nil {
		return Payment{}, err
	}
	if request.OrderID == uuid.Nil {
		return Payment{}, ErrInvalidOrderID
	}
	if request.IdempotencyKey == "" {
		return Payment{}, ErrIdempotencyKeyRequired
	}
	if !idempotency.ValidKey(request.IdempotencyKey) {
		return Payment{}, ErrInvalidIdempotencyKey
	}
	paymentID, err := uuid.NewRandom()
	if err != nil {
		return Payment{}, fmt.Errorf("generate payment ID: %w", err)
	}
	keyHash := idempotency.KeyHash(request.IdempotencyKey)
	requestHash := idempotency.RequestHash(idempotency.PaymentCreateOperation, request.OrderID[:])
	operationCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	created, err := s.repository.CreatePayment(operationCtx, actor.UserID, CreateParams{
		PaymentID: paymentID, OrderID: request.OrderID,
		KeyHash: keyHash[:], RequestHash: requestHash[:],
	})
	if err != nil {
		return Payment{}, err
	}
	message := "payment created"
	if created.IdempotencyReplay {
		message = "payment creation replayed"
	}
	s.logger.InfoContext(
		ctx, message,
		slog.String("payment_id", created.ID.String()),
		slog.String("order_id", created.OrderID.String()),
		slog.String("user_id", actor.UserID.String()),
	)
	return created, nil
}

func (s *Service) GetPayment(ctx context.Context, actor Actor, paymentID uuid.UUID) (Payment, error) {
	if err := validateActor(actor, ""); err != nil {
		return Payment{}, err
	}
	if paymentID == uuid.Nil {
		return Payment{}, ErrPaymentNotFound
	}
	operationCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	return s.repository.GetPayment(operationCtx, actor, paymentID)
}

func validateActor(actor Actor, requiredRole string) error {
	if actor.UserID == uuid.Nil || (actor.Role != RoleCustomer && actor.Role != RoleAdmin) {
		return ErrInvalidActor
	}
	if requiredRole != "" && actor.Role != requiredRole {
		return ErrForbidden
	}
	return nil
}
