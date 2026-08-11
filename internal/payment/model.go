package payment

import (
	"time"

	"github.com/google/uuid"
)

const (
	RoleCustomer = "customer"
	RoleAdmin    = "admin"
)

type Status string

const StatusSucceeded Status = "succeeded"

type Actor struct {
	UserID uuid.UUID
	Role   string
}

type Payment struct {
	ID                uuid.UUID
	OrderID           uuid.UUID
	UserID            uuid.UUID
	AccountID         uuid.UUID
	Status            Status
	Currency          string
	Amount            int64
	BalanceBefore     int64
	BalanceAfter      int64
	CreatedAt         time.Time
	IdempotencyReplay bool
}

type CreateRequest struct {
	OrderID        uuid.UUID
	IdempotencyKey string
}

type CreateParams struct {
	PaymentID   uuid.UUID
	OrderID     uuid.UUID
	KeyHash     []byte
	RequestHash []byte
}
