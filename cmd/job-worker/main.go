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

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"

	"github.com/assumeengagetry/distributed-commerce/internal/config"
	"github.com/assumeengagetry/distributed-commerce/internal/database"
	"github.com/assumeengagetry/distributed-commerce/internal/jobs"
	"github.com/assumeengagetry/distributed-commerce/internal/observability"
	"github.com/assumeengagetry/distributed-commerce/internal/platform/metricsserver"
	"github.com/assumeengagetry/distributed-commerce/internal/platform/process"
	"github.com/assumeengagetry/distributed-commerce/internal/redisclient"
)

func main() {
	if err := run(); err != nil {
		logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
		logger.Error("job worker stopped", slog.Any("error", err))
		os.Exit(1)
	}
}

func run() (result error) {
	cfg, err := config.LoadJobWorker()
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
	queueRedis, err := redisclient.New(cfg.Redis)
	if err != nil {
		return fmt.Errorf("create queue Redis client: %w", err)
	}
	defer queueRedis.Close()
	if err := redisclient.Instrument(queueRedis, telemetry, "queue", false); err != nil {
		return fmt.Errorf("instrument queue Redis client: %w", err)
	}
	pingCtx, pingCancel := context.WithTimeout(ctx, cfg.Redis.DialTimeout)
	err = queueRedis.Ping(pingCtx).Err()
	pingCancel()
	if err != nil {
		return fmt.Errorf("ping queue Redis: %w", err)
	}

	handler, err := jobs.NewHandler(
		database.NewUserRepository(pool, cfg.Database.LockTimeout, cfg.Database.OperationTimeout),
		logger,
		jobs.HandlerConfig{
			DatabaseTimeout: cfg.Database.OperationTimeout,
			TaskTimeout:     cfg.TaskTimeout,
			BatchSize:       cfg.SessionCleanupBatchSize,
		},
		telemetry,
	)
	if err != nil {
		return fmt.Errorf("create job handler: %w", err)
	}
	return process.Run(
		ctx,
		func(ctx context.Context) error {
			return serveJobs(ctx, cfg, logger, queueRedis, handler, stop)
		},
		func(ctx context.Context) error {
			return metricsserver.Serve(ctx, cfg.Telemetry.Metrics, logger, telemetryRuntime.MetricsHandler(), nil)
		},
	)
}

func serveJobs(
	ctx context.Context,
	cfg config.JobWorkerConfig,
	logger *slog.Logger,
	queueRedis *redis.Client,
	handler *jobs.Handler,
	shutdownStarted func(),
) error {
	mux := asynq.NewServeMux()
	handler.Register(mux)
	asynqLogger := jobs.NewLogger(logger)
	server := asynq.NewServerFromRedisClient(queueRedis, asynq.Config{
		Concurrency:     cfg.Concurrency,
		Queues:          map[string]int{cfg.Queue: 1},
		ShutdownTimeout: cfg.ShutdownTimeout,
		ErrorHandler:    jobs.NewErrorHandler(logger),
		Logger:          asynqLogger,
		HealthCheckFunc: func(err error) {
			if err != nil {
				logger.Warn("job queue health check failed", slog.Any("error", err))
			}
		},
	})
	if err := server.Start(mux); err != nil {
		return fmt.Errorf("start job worker: %w", err)
	}
	scheduler := asynq.NewSchedulerFromRedisClient(queueRedis, &asynq.SchedulerOpts{
		Location: time.UTC,
		Logger:   asynqLogger,
		PostEnqueueFunc: func(_ *asynq.TaskInfo, err error) {
			if err != nil {
				logger.Warn("scheduled session cleanup enqueue failed", slog.Any("error", err))
			}
		},
	})
	if _, err := scheduler.Register(
		"@every "+cfg.CleanupInterval.String(), jobs.NewSessionCleanupTask(),
		jobs.SessionCleanupOptions(cfg.Queue, cfg.TaskTimeout)...,
	); err != nil {
		server.Shutdown()
		return fmt.Errorf("register scheduled session cleanup: %w", err)
	}
	if err := scheduler.Start(); err != nil {
		server.Shutdown()
		return fmt.Errorf("start job scheduler: %w", err)
	}
	logger.Info("job worker started",
		slog.String("queue", cfg.Queue), slog.Int("concurrency", cfg.Concurrency),
		slog.Duration("cleanup_interval", cfg.CleanupInterval),
	)
	<-ctx.Done()
	if shutdownStarted != nil {
		shutdownStarted()
	}
	logger.Info("job worker shutdown signal received")
	scheduler.Shutdown()
	server.Shutdown()
	logger.Info("job worker stopped")
	return nil
}
