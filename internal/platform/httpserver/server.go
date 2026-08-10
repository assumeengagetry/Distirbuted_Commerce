package httpserver

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"

	"github.com/assumeengagetry/distributed-commerce/internal/config"
)

func Serve(
	ctx context.Context,
	cfg config.HTTPConfig,
	logger *slog.Logger,
	handler http.Handler,
	shutdownStarted func(),
) error {
	if ctx == nil || logger == nil || handler == nil {
		return fmt.Errorf("HTTP server context, logger, and handler are required")
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS13}
	if cfg.TLSCertFile != "" {
		certificate, err := tls.LoadX509KeyPair(cfg.TLSCertFile, cfg.TLSKeyFile)
		if err != nil {
			return fmt.Errorf("load HTTP TLS certificate: %w", err)
		}
		tlsConfig.Certificates = []tls.Certificate{certificate}
	}

	server := &http.Server{
		Addr:              cfg.Address,
		Handler:           handler,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		MaxHeaderBytes:    1 << 20,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
		TLSConfig:         tlsConfig,
	}
	listener, err := net.Listen("tcp", cfg.Address)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.Address, err)
	}

	serverErrors := make(chan error, 1)
	go func() {
		tlsEnabled := cfg.TLSCertFile != ""
		logger.InfoContext(
			ctx, "HTTP server listening",
			slog.String("address", listener.Addr().String()), slog.Bool("tls", tlsEnabled),
		)
		if tlsEnabled {
			serverErrors <- server.ServeTLS(listener, "", "")
			return
		}
		serverErrors <- server.Serve(listener)
	}()

	select {
	case err := <-serverErrors:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return errors.Join(fmt.Errorf("serve HTTP: %w", err), server.Close())
		}
		return nil
	case <-ctx.Done():
		if shutdownStarted != nil {
			shutdownStarted()
		}
		logger.Info("shutdown signal received")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		return errors.Join(fmt.Errorf("gracefully shut down HTTP server: %w", err), server.Close())
	}
	if err := <-serverErrors; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("stop HTTP server: %w", err)
	}
	logger.Info("HTTP server stopped")
	return nil
}
