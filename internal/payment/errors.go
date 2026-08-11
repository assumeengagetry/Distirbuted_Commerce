package payment

import "errors"

var (
	ErrInvalidActor            = errors.New("invalid actor")
	ErrForbidden               = errors.New("permission denied")
	ErrAccountDisabled         = errors.New("account is disabled")
	ErrInvalidOrderID          = errors.New("order ID is invalid")
	ErrIdempotencyKeyRequired  = errors.New("Idempotency-Key header is required")
	ErrInvalidIdempotencyKey   = errors.New("Idempotency-Key header is invalid")
	ErrIdempotencyConflict     = errors.New("idempotency key was used for a different request")
	ErrIdempotencyInProgress   = errors.New("idempotent operation is still in progress")
	ErrOperationOutcomeUnknown = errors.New("operation outcome is unknown")
	ErrOrderNotFound           = errors.New("order not found")
	ErrOrderAlreadyPaid        = errors.New("order is already paid")
	ErrOrderNotPayable         = errors.New("order is not payable")
	ErrInsufficientFunds       = errors.New("insufficient account funds")
	ErrCurrencyMismatch        = errors.New("order and account currencies do not match")
	ErrPaymentNotFound         = errors.New("payment not found")
	ErrTemporarilyUnavailable  = errors.New("service temporarily unavailable")
)
