package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"google.golang.org/grpc"

	"github.com/assumeengagetry/distributed-commerce/internal/auth"
	"github.com/assumeengagetry/distributed-commerce/internal/config"
	"github.com/assumeengagetry/distributed-commerce/internal/database"
	store "github.com/assumeengagetry/distributed-commerce/internal/database/sqlc"
	identityv1 "github.com/assumeengagetry/distributed-commerce/internal/genproto/identity/v1"
	"github.com/assumeengagetry/distributed-commerce/internal/jobs"
	"github.com/assumeengagetry/distributed-commerce/internal/observability"
	"github.com/assumeengagetry/distributed-commerce/internal/platform/grpcserver"
	"github.com/assumeengagetry/distributed-commerce/internal/platform/httpserver"
	"github.com/assumeengagetry/distributed-commerce/internal/platform/metricsserver"
	"github.com/assumeengagetry/distributed-commerce/internal/platform/process"
	"github.com/assumeengagetry/distributed-commerce/internal/redisclient"
	grpctransport "github.com/assumeengagetry/distributed-commerce/internal/transport/grpc"
	httptransport "github.com/assumeengagetry/distributed-commerce/internal/transport/http"
	"github.com/assumeengagetry/distributed-commerce/internal/user"
)

func main() {
	if err := run(); err != nil {
		logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
		logger.Error("user service stopped", slog.Any("error", err))
		os.Exit(1)
	}
}

func run() (result error) {
	cfg, err := config.LoadForService("user-service", "127.0.0.1:8081")
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
	var grpcListening atomic.Bool
	readinessCheck := func(ctx context.Context) error {
		value, err := queries.HealthCheck(ctx)
		if err != nil {
			return err
		}
		if value != 1 {
			return fmt.Errorf("unexpected health check result: %d", value)
		}
		return nil
	}

	passwords, err := auth.NewPasswordHasher(auth.PasswordParams{
		MemoryKiB:      cfg.Auth.Argon2MemoryKiB,
		Iterations:     cfg.Auth.Argon2Iterations,
		Parallelism:    cfg.Auth.Argon2Parallelism,
		SaltLength:     16,
		KeyLength:      32,
		MaxConcurrency: cfg.Auth.Argon2MaxConcurrency,
	})
	if err != nil {
		return fmt.Errorf("create password hasher: %w", err)
	}
	tokens, err := auth.NewTokenManager(
		cfg.Auth.PasetoV4LocalKey,
		cfg.Auth.Issuer,
		cfg.Auth.AccessTokenTTL,
		cfg.Auth.ClockSkew,
	)
	if err != nil {
		return fmt.Errorf("create token manager: %w", err)
	}
	var cleanupScheduler user.SessionCleanupScheduler
	if cfg.Jobs.Enabled {
		queueRedis, err := redisclient.New(cfg.Jobs.Redis)
		if err != nil {
			return fmt.Errorf("create queue Redis client: %w", err)
		}
		defer queueRedis.Close()
		if err := redisclient.Instrument(queueRedis, telemetry, "queue", false); err != nil {
			return fmt.Errorf("instrument queue Redis client: %w", err)
		}
		cleanupScheduler, err = jobs.NewClient(queueRedis, jobs.ClientConfig{
			Queue: cfg.Jobs.Queue, EnqueueTimeout: cfg.Jobs.EnqueueTimeout,
			TaskTimeout: cfg.Jobs.TaskTimeout, UniqueTTL: cfg.Jobs.UniqueTTL,
		}, telemetry)
		if err != nil {
			return fmt.Errorf("create job client: %w", err)
		}
	}
	userService, err := user.NewService(
		database.NewUserRepository(pool, cfg.Database.LockTimeout, cfg.Database.OperationTimeout),
		passwords,
		tokens,
		logger,
		user.ServiceConfig{
			RefreshTTL:        cfg.Auth.RefreshTokenTTL,
			RefreshReuseGrace: cfg.Auth.RefreshReuseGrace,
			DatabaseTimeout:   cfg.Database.OperationTimeout,
			Now:               time.Now,
			SessionCleanup:    cleanupScheduler,
		},
	)
	if err != nil {
		return fmt.Errorf("create user service: %w", err)
	}

	router, err := httptransport.NewRouter(httptransport.Dependencies{
		Logger:      logger,
		ServiceName: cfg.ServiceName,
		ReadinessChecks: map[string]func(context.Context) error{
			"postgres": readinessCheck,
			"identity_grpc": func(context.Context) error {
				if !grpcListening.Load() {
					return fmt.Errorf("identity gRPC listener is not ready")
				}
				return nil
			},
		},
		ReadinessTimeout: cfg.Database.PingTimeout,
		UserService:      userService,
		TokenVerifier:    tokens,
		AuthRateLimit: httptransport.RateLimitConfig{
			RequestsPerSecond: cfg.Auth.RateLimit.RequestsPerSecond,
			Burst:             cfg.Auth.RateLimit.Burst,
			EntryTTL:          cfg.Auth.RateLimit.EntryTTL,
			MaxEntries:        cfg.Auth.RateLimit.MaxEntries,
		},
		Now:       time.Now,
		Telemetry: telemetry,
	})
	if err != nil {
		return fmt.Errorf("create HTTP router: %w", err)
	}
	identityServer, err := grpctransport.NewIdentityServer(tokens, time.Now)
	if err != nil {
		return fmt.Errorf("create identity gRPC service: %w", err)
	}
	return process.Run(
		ctx,
		func(ctx context.Context) error {
			return httpserver.Serve(ctx, cfg.HTTP, logger, router, stop)
		},
		func(ctx context.Context) error {
			return grpcserver.Serve(ctx, cfg.GRPC, logger, telemetry, func(registrar grpc.ServiceRegistrar) {
				identityv1.RegisterIdentityServiceServer(registrar, identityServer)
			}, func() { grpcListening.Store(true) }, func() { grpcListening.Store(false) })
		},
		func(ctx context.Context) error {
			return metricsserver.Serve(ctx, cfg.Telemetry.Metrics, logger, telemetryRuntime.MetricsHandler(), nil)
		},
	)
}
