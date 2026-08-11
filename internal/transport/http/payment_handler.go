package httptransport

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/assumeengagetry/distributed-commerce/internal/auth"
	"github.com/assumeengagetry/distributed-commerce/internal/payment"
)

type paymentHandler struct {
	service PaymentService
	logger  *slog.Logger
}

type createPaymentBody struct {
	OrderID uuid.UUID `json:"order_id"`
}

type paymentResponse struct {
	ID        uuid.UUID      `json:"id"`
	OrderID   uuid.UUID      `json:"order_id"`
	UserID    uuid.UUID      `json:"user_id"`
	Status    payment.Status `json:"status"`
	Currency  string         `json:"currency"`
	Amount    int64          `json:"amount"`
	CreatedAt time.Time      `json:"created_at"`
}

func (h paymentHandler) create(c *gin.Context) {
	idempotencyKey, err := idempotencyKeyFromRequest(c)
	if err != nil {
		writeIdempotencyHeaderError(c, err)
		return
	}
	var body createPaymentBody
	if err := decodeJSON(c, &body); err != nil {
		writeDecodeError(c, err)
		return
	}
	actor, ok := paymentActorFromContext(c)
	if !ok {
		return
	}
	created, err := h.service.CreatePayment(c.Request.Context(), actor, payment.CreateRequest{
		OrderID: body.OrderID, IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		h.writeServiceError(c, err)
		return
	}
	c.Header("Location", "/v1/payments/"+created.ID.String())
	if created.IdempotencyReplay {
		c.Header("Idempotency-Replayed", "true")
	}
	c.JSON(http.StatusCreated, gin.H{"payment": paymentResult(created)})
}

func (h paymentHandler) get(c *gin.Context) {
	paymentID, ok := pathUUID(c, "payment_id", "INVALID_PAYMENT_ID")
	if !ok {
		return
	}
	actor, ok := paymentActorFromContext(c)
	if !ok {
		return
	}
	result, err := h.service.GetPayment(c.Request.Context(), actor, paymentID)
	if err != nil {
		h.writeServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"payment": paymentResult(result)})
}

func (h paymentHandler) writeServiceError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, payment.ErrInvalidOrderID):
		writeAPIError(c, http.StatusBadRequest, "INVALID_ORDER_ID", "invalid order ID")
	case errors.Is(err, payment.ErrIdempotencyKeyRequired):
		writeAPIError(c, http.StatusBadRequest, "IDEMPOTENCY_KEY_REQUIRED", "Idempotency-Key header is required")
	case errors.Is(err, payment.ErrInvalidIdempotencyKey):
		writeAPIError(c, http.StatusBadRequest, "INVALID_IDEMPOTENCY_KEY", "Idempotency-Key header is invalid")
	case errors.Is(err, payment.ErrInvalidActor):
		c.Header("WWW-Authenticate", "Bearer")
		writeAPIError(c, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
	case errors.Is(err, payment.ErrForbidden):
		writeAPIError(c, http.StatusForbidden, "FORBIDDEN", "permission denied")
	case errors.Is(err, payment.ErrAccountDisabled):
		c.Header("WWW-Authenticate", "Bearer")
		writeAPIError(c, http.StatusUnauthorized, "ACCOUNT_DISABLED", "account is disabled")
	case errors.Is(err, payment.ErrOrderNotFound):
		writeAPIError(c, http.StatusNotFound, "ORDER_NOT_FOUND", "order not found")
	case errors.Is(err, payment.ErrPaymentNotFound):
		writeAPIError(c, http.StatusNotFound, "PAYMENT_NOT_FOUND", "payment not found")
	case errors.Is(err, payment.ErrInsufficientFunds):
		writeAPIError(c, http.StatusConflict, "INSUFFICIENT_FUNDS", "insufficient account funds")
	case errors.Is(err, payment.ErrOrderAlreadyPaid):
		writeAPIError(c, http.StatusConflict, "ORDER_ALREADY_PAID", "order is already paid")
	case errors.Is(err, payment.ErrOrderNotPayable):
		writeAPIError(c, http.StatusConflict, "ORDER_NOT_PAYABLE", "order is not payable")
	case errors.Is(err, payment.ErrCurrencyMismatch):
		writeAPIError(c, http.StatusConflict, "CURRENCY_MISMATCH", "order and account currencies do not match")
	case errors.Is(err, payment.ErrIdempotencyConflict):
		writeAPIError(c, http.StatusConflict, "IDEMPOTENCY_KEY_REUSED", "idempotency key was used for a different request")
	case errors.Is(err, payment.ErrIdempotencyInProgress):
		c.Header("Retry-After", "1")
		writeAPIError(c, http.StatusConflict, "IDEMPOTENCY_IN_PROGRESS", "idempotent operation is still in progress")
	case errors.Is(err, payment.ErrOperationOutcomeUnknown):
		c.Header("Retry-After", "1")
		writeAPIError(c, http.StatusServiceUnavailable, "OPERATION_OUTCOME_UNKNOWN", "retry the same request with the same idempotency key")
	case errors.Is(err, payment.ErrTemporarilyUnavailable):
		c.Header("Retry-After", "1")
		writeAPIError(c, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "service temporarily unavailable")
	default:
		h.logger.ErrorContext(
			c.Request.Context(), "payment request failed",
			slog.String("request_id", requestIDFromContext(c)), slog.Any("error", err),
		)
		writeAPIError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "internal server error")
	}
}

func paymentActorFromContext(c *gin.Context) (payment.Actor, bool) {
	principal, ok := auth.PrincipalFromContext(c.Request.Context())
	if !ok {
		writeAPIError(c, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
		return payment.Actor{}, false
	}
	return payment.Actor{UserID: principal.UserID, Role: principal.Role}, true
}

func paymentResult(result payment.Payment) paymentResponse {
	return paymentResponse{
		ID: result.ID, OrderID: result.OrderID, UserID: result.UserID,
		Status: result.Status, Currency: result.Currency, Amount: result.Amount, CreatedAt: result.CreatedAt,
	}
}
