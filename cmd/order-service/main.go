package main

import (
	"context"
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
	commerce "github.com/assumeengagetry/distributed-commerce/internal/order"
	"github.com/assumeengagetry/distributed-commerce/internal/platform/httpserver"
	"github.com/assumeengagetry/distributed-commerce/internal/productcache"
	"github.com/assumeengagetry/distributed-commerce/internal/redisclient"
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
	postgresReadiness := func(ctx context.Context) error {
		value, err := queries.OrderHealthCheck(ctx)
		if err != nil {
			return err
		}
		if value != 1 {
			return fmt.Errorf("unexpected order health check result: %d", value)
		}
		return nil
	}

	identityClient, err := identity.Dial(identity.ClientConfig{
		Target: cfg.GRPC.IdentityTarget, Timeout: cfg.GRPC.CallTimeout,
		TLSCertFile: cfg.GRPC.TLSCertFile, TLSKeyFile: cfg.GRPC.TLSKeyFile,
		TLSCAFile: cfg.GRPC.TLSCAFile, TLSServerName: cfg.GRPC.TLSServerName,
		Logger: logger,
	})
	if err != nil {
		return fmt.Errorf("create identity gRPC client: %w", err)
	}
	defer identityClient.Close()
	var repository commerce.Repository = database.NewOrderRepository(
		pool, cfg.Database.LockTimeout, cfg.Database.OperationTimeout, cfg.Database.CommitResolutionTimeout,
	)
	if cfg.ProductCache.Enabled {
		cacheClient, err := redisclient.New(cfg.ProductCache.Redis)
		if err != nil {
			return fmt.Errorf("create product cache Redis client: %w", err)
		}
		defer cacheClient.Close()
		repository, err = productcache.NewRepository(
			repository, cacheClient, logger, cfg.ProductCache.TTL, cfg.ProductCache.OperationTimeout,
		)
		if err != nil {
			return fmt.Errorf("create cached order repository: %w", err)
		}
	}
	serviceTimeout := cfg.Database.OperationTimeout
	if cfg.ProductCache.Enabled {
		serviceTimeout += 2 * cfg.ProductCache.OperationTimeout
	}
	service, err := commerce.NewService(repository, logger, serviceTimeout)
	if err != nil {
		return fmt.Errorf("create commerce service: %w", err)
	}
	router, err := httptransport.NewOrderRouter(httptransport.OrderDependencies{
		Logger: logger, ServiceName: cfg.ServiceName,
		ReadinessChecks: map[string]func(context.Context) error{
			"postgres": postgresReadiness, "identity_grpc": identityClient.Health,
		},
		ReadinessTimeout: cfg.Database.PingTimeout,
		CommerceService:  service, TokenVerifier: identityClient,
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
