package payment

import (
	"context"

	"github.com/google/uuid"
)

type Repository interface {
	CreatePayment(ctx context.Context, actorID uuid.UUID, params CreateParams) (Payment, error)
	GetPayment(ctx context.Context, actor Actor, paymentID uuid.UUID) (Payment, error)
}
