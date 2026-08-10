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
	"github.com/assumeengagetry/distributed-commerce/internal/platform/httpserver"
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

func run() error {
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

	pool, err := database.Open(ctx, cfg.Database)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer pool.Close()

	queries := store.New(pool)
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
		},
	)
	if err != nil {
		return fmt.Errorf("create user service: %w", err)
	}

	router, err := httptransport.NewRouter(httptransport.Dependencies{
		Logger:           logger,
		ServiceName:      cfg.ServiceName,
		ReadinessCheck:   readinessCheck,
		ReadinessTimeout: cfg.Database.PingTimeout,
		UserService:      userService,
		TokenVerifier:    tokens,
		AuthRateLimit: httptransport.RateLimitConfig{
			RequestsPerSecond: cfg.Auth.RateLimit.RequestsPerSecond,
			Burst:             cfg.Auth.RateLimit.Burst,
			EntryTTL:          cfg.Auth.RateLimit.EntryTTL,
			MaxEntries:        cfg.Auth.RateLimit.MaxEntries,
		},
		Now: time.Now,
	})
	if err != nil {
		return fmt.Errorf("create HTTP router: %w", err)
	}

	return httpserver.Serve(ctx, cfg.HTTP, logger, router, stop)
}
