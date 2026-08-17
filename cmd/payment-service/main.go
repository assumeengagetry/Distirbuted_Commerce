package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/assumeengagetry/distributed-commerce/internal/config"
	"github.com/assumeengagetry/distributed-commerce/internal/database"
	store "github.com/assumeengagetry/distributed-commerce/internal/database/sqlc"
	"github.com/assumeengagetry/distributed-commerce/internal/identity"
	"github.com/assumeengagetry/distributed-commerce/internal/observability"
	"github.com/assumeengagetry/distributed-commerce/internal/payment"
	"github.com/assumeengagetry/distributed-commerce/internal/platform/httpserver"
	"github.com/assumeengagetry/distributed-commerce/internal/platform/metricsserver"
	"github.com/assumeengagetry/distributed-commerce/internal/platform/process"
	httptransport "github.com/assumeengagetry/distributed-commerce/internal/transport/http"
)

func main() {
	if err := run(); err != nil {
		logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
		logger.Error("payment service stopped", slog.Any("error", err))
		os.Exit(1)
	}
}

func run() (result error) {
	cfg, err := config.LoadForService("payment-service", "127.0.0.1:8083")
	if err != nil {
		return err
	}
	logger, err := observability.NewLogger(os.Stdout, cfg.Log, cfg.ServiceName, cfg.Environment)
	if err != nil {
		return fmt.Errorf("create logger: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	telemetryRuntime, err := observability.NewRuntime(ctx, cfg.Telemetry, cfg.ServiceName, cfg.Environment)
	if err != nil {
		return fmt.Errorf("create telemetry runtime: %w", err)
	}
	defer func() {
		if err := telemetryRuntime.ShutdownWithin(cfg.Telemetry.ShutdownTimeout); err != nil {
			result = errors.Join(result, fmt.Errorf("shutdown telemetry: %w", err))
		}
	}()
	telemetry := telemetryRuntime.Providers()
	pool, err := database.Open(ctx, cfg.Database, telemetry)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer pool.Close()
	queries := store.New(pool)
	postgresReadiness := func(ctx context.Context) error {
		value, err := queries.PaymentHealthCheck(ctx)
		if err != nil {
			return err
		}
		if value != 1 {
			return fmt.Errorf("unexpected payment health check result: %d", value)
		}
		return nil
	}
	identityClient, err := identity.Dial(identity.ClientConfig{
		Target: cfg.GRPC.IdentityTarget, Timeout: cfg.GRPC.CallTimeout,
		TLSCertFile: cfg.GRPC.TLSCertFile, TLSKeyFile: cfg.GRPC.TLSKeyFile,
		TLSCAFile: cfg.GRPC.TLSCAFile, TLSServerName: cfg.GRPC.TLSServerName,
		Logger:    logger,
		Telemetry: telemetry,
	})
	if err != nil {
		return fmt.Errorf("create identity gRPC client: %w", err)
	}
	defer identityClient.Close()
	service, err := payment.NewService(
		database.NewPaymentRepository(
			pool, cfg.Database.LockTimeout, cfg.Database.OperationTimeout, cfg.Database.CommitResolutionTimeout,
		),
		logger, cfg.Database.OperationTimeout,
	)
	if err != nil {
		return fmt.Errorf("create payment service: %w", err)
	}
	router, err := httptransport.NewPaymentRouter(httptransport.PaymentDependencies{
		Logger: logger, ServiceName: cfg.ServiceName,
		ReadinessChecks: map[string]func(context.Context) error{
			"postgres": postgresReadiness, "identity_grpc": identityClient.Health,
		},
		ReadinessTimeout: cfg.Database.PingTimeout,
		PaymentService:   service, TokenVerifier: identityClient,
		RateLimit: httptransport.RateLimitConfig{
			RequestsPerSecond: cfg.Payment.RateLimit.RequestsPerSecond,
			Burst:             cfg.Payment.RateLimit.Burst, EntryTTL: cfg.Payment.RateLimit.EntryTTL,
			MaxEntries: cfg.Payment.RateLimit.MaxEntries,
		},
		Now:       time.Now,
		Telemetry: telemetry,
	})
	if err != nil {
		return fmt.Errorf("create payment HTTP router: %w", err)
	}
	return process.Run(
		ctx,
		func(ctx context.Context) error {
			return httpserver.Serve(ctx, cfg.HTTP, logger, router, stop)
		},
		func(ctx context.Context) error {
			return metricsserver.Serve(ctx, cfg.Telemetry.Metrics, logger, telemetryRuntime.MetricsHandler(), nil)
		},
	)
}
