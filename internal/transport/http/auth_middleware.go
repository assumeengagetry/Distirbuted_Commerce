package httptransport

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/assumeengagetry/distributed-commerce/internal/auth"
)

type AccessTokenVerifier interface {
	ParseAccess(encoded string, now time.Time) (auth.Principal, error)
}

func authenticate(verifier AccessTokenVerifier, now func() time.Time) gin.HandlerFunc {
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
		principal, err := verifier.ParseAccess(token, now().UTC())
		if err != nil {
			c.Header("WWW-Authenticate", "Bearer")
			writeAPIError(c, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
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
