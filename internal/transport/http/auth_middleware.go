package httptransport

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/assumeengagetry/distributed-commerce/internal/auth"
)

type AccessTokenVerifier interface {
	VerifyAccess(context.Context, string) (auth.Principal, error)
}

func authenticate(verifier AccessTokenVerifier) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		headers := c.Request.Header.Values("Authorization")
		if len(headers) != 1 {
			c.Header("WWW-Authenticate", "Bearer")
			writeAPIError(c, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
			return
		}
		token, ok := bearerToken(headers[0])
		if !ok {
			c.Header("WWW-Authenticate", "Bearer")
			writeAPIError(c, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
			return
		}
		principal, err := verifier.VerifyAccess(c.Request.Context(), token)
		if err != nil {
			if errors.Is(err, auth.ErrInvalidAccessToken) {
				c.Header("WWW-Authenticate", "Bearer")
				writeAPIError(c, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
				return
			}
			c.Header("Retry-After", "1")
			writeAPIError(c, http.StatusServiceUnavailable, "IDENTITY_UNAVAILABLE", "identity service temporarily unavailable")
			return
		}

		ctx := auth.ContextWithPrincipal(c.Request.Context(), principal)
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}

func requireRoles(roles ...string) gin.HandlerFunc {
	allowed := make(map[string]struct{}, len(roles))
	for _, role := range roles {
		allowed[role] = struct{}{}
	}
	return func(c *gin.Context) {
		principal, ok := auth.PrincipalFromContext(c.Request.Context())
		if !ok {
			writeAPIError(c, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
			return
		}
		if _, ok := allowed[principal.Role]; !ok {
			writeAPIError(c, http.StatusForbidden, "FORBIDDEN", "permission denied")
			return
		}
		c.Next()
	}
}
