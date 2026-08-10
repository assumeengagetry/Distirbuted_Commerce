package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/assumeengagetry/distributed-commerce/internal/auth"
	"github.com/assumeengagetry/distributed-commerce/internal/config"
	"github.com/assumeengagetry/distributed-commerce/internal/database"
	store "github.com/assumeengagetry/distributed-commerce/internal/database/sqlc"
	"github.com/assumeengagetry/distributed-commerce/internal/observability"
	commerce "github.com/assumeengagetry/distributed-commerce/internal/order"
	"github.com/assumeengagetry/distributed-commerce/internal/platform/httpserver"
	httptransport "github.com/assumeengagetry/distributed-commerce/internal/transport/http"
)

func main() {
	if err := run(); err != nil {
		logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
		logger.Error("order service stopped", slog.Any("error", err))
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.LoadForService("order-service", "127.0.0.1:8082")
	if err != nil {
		return err
	}
	logger, err := observability.NewLogger(os.Stdout, cfg.Log, cfg.ServiceName, cfg.Environment)
	if err != nil {
		return fmt.Errorf("create logger: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := database.Open(ctx, cfg.Database)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer pool.Close()
	queries := store.New(pool)
	readinessCheck := func(ctx context.Context) error {
		value, err := queries.OrderHealthCheck(ctx)
		if err != nil {
			return err
		}
		if value != 1 {
			return fmt.Errorf("unexpected order health check result: %d", value)
		}
		return nil
	}

	tokens, err := auth.NewTokenManager(
		cfg.Auth.PasetoV4LocalKey, cfg.Auth.Issuer, cfg.Auth.AccessTokenTTL, cfg.Auth.ClockSkew,
	)
	if err != nil {
		return fmt.Errorf("create token manager: %w", err)
	}
	service, err := commerce.NewService(
		database.NewOrderRepository(pool, cfg.Database.LockTimeout, cfg.Database.OperationTimeout),
		logger,
		cfg.Database.OperationTimeout,
	)
	if err != nil {
		return fmt.Errorf("create commerce service: %w", err)
	}
	router, err := httptransport.NewOrderRouter(httptransport.OrderDependencies{
		Logger: logger, ServiceName: cfg.ServiceName,
		ReadinessCheck: readinessCheck, ReadinessTimeout: cfg.Database.PingTimeout,
		CommerceService: service, TokenVerifier: tokens,
		RateLimit: httptransport.RateLimitConfig{
			RequestsPerSecond: cfg.Commerce.RateLimit.RequestsPerSecond,
			Burst:             cfg.Commerce.RateLimit.Burst, EntryTTL: cfg.Commerce.RateLimit.EntryTTL,
			MaxEntries: cfg.Commerce.RateLimit.MaxEntries,
		},
		Now: time.Now,
	})
	if err != nil {
		return fmt.Errorf("create order HTTP router: %w", err)
	}
	return httpserver.Serve(ctx, cfg.HTTP, logger, router, stop)
}
