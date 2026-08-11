package httptransport

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

type healthHandler struct {
	serviceName     string
	readinessChecks map[string]func(context.Context) error
	checkTimeout    time.Duration
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
	type checkResult struct {
		name string
		err  error
	}
	results := make(chan checkResult, len(h.readinessChecks))
	pending := make(map[string]struct{}, len(h.readinessChecks))
	for name, check := range h.readinessChecks {
		pending[name] = struct{}{}
		go func() { results <- checkResult{name: name, err: check(ctx)} }()
	}
	checks := make(map[string]string, len(h.readinessChecks))
	ready := true
	for len(pending) > 0 {
		select {
		case result := <-results:
			delete(pending, result.name)
			if result.err != nil {
				checks[result.name] = "down"
				ready = false
				continue
			}
			checks[result.name] = "up"
		case <-ctx.Done():
			for name := range pending {
				checks[name] = "down"
				delete(pending, name)
			}
			ready = false
		}
	}
	if !ready {
		c.JSON(http.StatusServiceUnavailable, healthResponse{
			Status:  "not_ready",
			Service: h.serviceName,
			Checks:  checks,
		})
		return
	}

	c.JSON(http.StatusOK, healthResponse{
		Status:  "ready",
		Service: h.serviceName,
		Checks:  checks,
	})
}
