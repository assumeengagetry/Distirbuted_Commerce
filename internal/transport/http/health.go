package httptransport

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

type healthHandler struct {
	serviceName    string
	readinessCheck func(context.Context) error
	checkTimeout   time.Duration
}

type healthResponse struct {
	Status  string            `json:"status"`
	Service string            `json:"service"`
	Checks  map[string]string `json:"checks,omitempty"`
}

func (h healthHandler) register(router gin.IRoutes) {
	router.GET("/healthz", h.liveness)
	router.GET("/readyz", h.readiness)
}

func (h healthHandler) liveness(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, healthResponse{
		Status:  "ok",
		Service: h.serviceName,
	})
}

func (h healthHandler) readiness(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), h.checkTimeout)
	defer cancel()

	c.Header("Cache-Control", "no-store")
	if err := h.readinessCheck(ctx); err != nil {
		c.JSON(http.StatusServiceUnavailable, healthResponse{
			Status:  "not_ready",
			Service: h.serviceName,
			Checks:  map[string]string{"postgres": "down"},
		})
		return
	}

	c.JSON(http.StatusOK, healthResponse{
		Status:  "ready",
		Service: h.serviceName,
		Checks:  map[string]string{"postgres": "up"},
	})
}
