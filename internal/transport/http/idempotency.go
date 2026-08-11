package httptransport

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
)

var (
	errIdempotencyKeyRequired = errors.New("Idempotency-Key header is required")
	errInvalidIdempotencyKey  = errors.New("Idempotency-Key header is invalid")
)

func idempotencyKeyFromRequest(c *gin.Context) (string, error) {
	values := c.Request.Header.Values("Idempotency-Key")
	if len(values) == 0 {
		return "", errIdempotencyKeyRequired
	}
	if len(values) != 1 || values[0] == "" {
		return "", errInvalidIdempotencyKey
	}
	return values[0], nil
}

func writeIdempotencyHeaderError(c *gin.Context, err error) {
	if errors.Is(err, errIdempotencyKeyRequired) {
		writeAPIError(c, http.StatusBadRequest, "IDEMPOTENCY_KEY_REQUIRED", "Idempotency-Key header is required")
		return
	}
	writeAPIError(c, http.StatusBadRequest, "INVALID_IDEMPOTENCY_KEY", "Idempotency-Key header is invalid")
}
