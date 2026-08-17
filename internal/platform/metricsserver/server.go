package metricsserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"github.com/assumeengagetry/distributed-commerce/internal/config"
)

func Serve(
	ctx context.Context,
	cfg config.MetricsConfig,
	logger *slog.Logger,
	metrics http.Handler,
	onListening func(net.Addr),
) error {
	if ctx == nil || logger == nil || metrics == nil {
		return fmt.Errorf("metrics server context, logger, and handler are required")
	}
	if cfg.Address == "" || cfg.ReadHeaderTimeout <= 0 || cfg.WriteTimeout <= 0 ||
		cfg.IdleTimeout <= 0 || cfg.ShutdownTimeout <= 0 {
		return fmt.Errorf("metrics server address and timeouts are required")
	}
	if !loopbackAddress(cfg.Address) {
		return fmt.Errorf("metrics server address must be loopback")
	}

	mux := http.NewServeMux()
	mux.Handle("GET /metrics", http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Cache-Control", "no-store")
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		metrics.ServeHTTP(writer, request)
	}))
	server := &http.Server{
		Addr:              cfg.Address,
		Handler:           mux,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}
	listener, err := net.Listen("tcp", cfg.Address)
	if err != nil {
		return fmt.Errorf("listen for metrics on %s: %w", cfg.Address, err)
	}
	if onListening != nil {
		onListening(listener.Addr())
	}
	serverErrors := make(chan error, 1)
	go func() {
		logger.InfoContext(ctx, "metrics server listening", slog.String("address", listener.Addr().String()))
		serverErrors <- server.Serve(listener)
	}()

	select {
	case err := <-serverErrors:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return errors.Join(fmt.Errorf("serve metrics: %w", err), server.Close())
		}
		return nil
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		return errors.Join(fmt.Errorf("gracefully shut down metrics server: %w", err), server.Close())
	}
	if err := <-serverErrors; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("stop metrics server: %w", err)
	}
	logger.Info("metrics server stopped")
	return nil
}

func loopbackAddress(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
