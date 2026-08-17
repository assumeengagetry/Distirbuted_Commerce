//go:build integration

package jobs

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"

	"github.com/assumeengagetry/distributed-commerce/internal/observability"
)

func TestRealRedisWorkerRetryArchiveAndRecovery(t *testing.T) {
	redisClient := integrationRedisClient(t)
	queue := "maintenance-test-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	var calls atomic.Int32
	repository := &stubSessionRepository{delete: func(context.Context, int32) (int64, error) {
		if calls.Add(1) == 1 {
			return 0, errors.New("temporary database failure")
		}
		return 3, nil
	}}
	handler, err := NewHandler(repository, logger, HandlerConfig{
		DatabaseTimeout: time.Second, TaskTimeout: 2 * time.Second, BatchSize: 100,
	}, observability.NoopProviders())
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	mux := asynq.NewServeMux()
	handler.Register(mux)
	server := asynq.NewServerFromRedisClient(redisClient, asynq.Config{
		Concurrency: 1, Queues: map[string]int{queue: 1}, ShutdownTimeout: time.Second,
		TaskCheckInterval: 10 * time.Millisecond, DelayedTaskCheckInterval: 100 * time.Millisecond,
		RetryDelayFunc: func(int, error, *asynq.Task) time.Duration { return 10 * time.Millisecond },
		ErrorHandler:   NewErrorHandler(logger), Logger: NewLogger(logger),
	})
	if err := server.Start(mux); err != nil {
		t.Fatalf("Server.Start() error = %v", err)
	}
	t.Cleanup(server.Shutdown)

	client, err := NewClient(redisClient, ClientConfig{
		Queue: queue, EnqueueTimeout: time.Second, TaskTimeout: time.Second, UniqueTTL: time.Second,
	}, observability.NoopProviders())
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	if err := client.ScheduleSessionCleanup(t.Context()); err != nil {
		t.Fatalf("ScheduleSessionCleanup() error = %v", err)
	}
	waitFor(t, 10*time.Second, func() bool { return calls.Load() >= 2 })

	rawClient := asynq.NewClientFromRedisClient(redisClient)
	if _, err := rawClient.Enqueue(
		asynq.NewTask(SessionCleanupTaskType, []byte(`{"schema":2}`)),
		asynq.Queue(queue), asynq.MaxRetry(5), asynq.Timeout(time.Second),
	); err != nil {
		t.Fatalf("enqueue malformed task: %v", err)
	}
	inspector := asynq.NewInspectorFromRedisClient(redisClient)
	var archived []*asynq.TaskInfo
	waitFor(t, 10*time.Second, func() bool {
		var inspectErr error
		archived, inspectErr = inspector.ListArchivedTasks(queue)
		return inspectErr == nil && len(archived) == 1
	})
	if archived[0].Type != SessionCleanupTaskType || archived[0].Retried != 0 {
		t.Fatalf("archived task = %+v", archived[0])
	}
	if err := inspector.RunTask(queue, archived[0].ID); err != nil {
		t.Fatalf("RunTask() error = %v", err)
	}
	waitFor(t, 10*time.Second, func() bool {
		tasks, inspectErr := inspector.ListArchivedTasks(queue)
		return inspectErr == nil && len(tasks) == 1
	})
	if err := inspector.DeleteTask(queue, archived[0].ID); err != nil {
		t.Fatalf("DeleteTask() error = %v", err)
	}
	waitFor(t, 5*time.Second, func() bool {
		tasks, inspectErr := inspector.ListArchivedTasks(queue)
		return inspectErr == nil && len(tasks) == 0
	})
}

func TestRealRedisRetryExhaustionArchivesTask(t *testing.T) {
	redisClient := integrationRedisClient(t)
	queue := "maintenance-exhaustion-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	var calls atomic.Int32
	repository := &stubSessionRepository{delete: func(context.Context, int32) (int64, error) {
		calls.Add(1)
		return 0, errors.New("persistent database failure")
	}}
	handler, err := NewHandler(repository, logger, HandlerConfig{
		DatabaseTimeout: time.Second, TaskTimeout: 2 * time.Second, BatchSize: 100,
	}, observability.NoopProviders())
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	mux := asynq.NewServeMux()
	handler.Register(mux)
	server := asynq.NewServerFromRedisClient(redisClient, asynq.Config{
		Concurrency: 1, Queues: map[string]int{queue: 1}, ShutdownTimeout: time.Second,
		TaskCheckInterval: 10 * time.Millisecond, DelayedTaskCheckInterval: 100 * time.Millisecond,
		RetryDelayFunc: func(int, error, *asynq.Task) time.Duration { return 10 * time.Millisecond },
		ErrorHandler:   NewErrorHandler(logger), Logger: NewLogger(logger),
	})
	if err := server.Start(mux); err != nil {
		t.Fatalf("Server.Start() error = %v", err)
	}
	t.Cleanup(server.Shutdown)
	client := asynq.NewClientFromRedisClient(redisClient)
	if _, err := client.Enqueue(
		NewSessionCleanupTask(), asynq.Queue(queue), asynq.MaxRetry(1), asynq.Timeout(2*time.Second),
	); err != nil {
		t.Fatalf("enqueue retry-exhaustion task: %v", err)
	}
	inspector := asynq.NewInspectorFromRedisClient(redisClient)
	var archived []*asynq.TaskInfo
	waitFor(t, 10*time.Second, func() bool {
		var inspectErr error
		archived, inspectErr = inspector.ListArchivedTasks(queue)
		return inspectErr == nil && len(archived) == 1
	})
	if calls.Load() != 2 || archived[0].Retried != 1 || archived[0].MaxRetry != 1 {
		t.Fatalf("retry exhaustion = calls:%d task:%+v", calls.Load(), archived[0])
	}
}

func integrationRedisClient(t *testing.T) *redis.Client {
	t.Helper()
	if strings.EqualFold(strings.TrimSpace(os.Getenv("APP_ENV")), "production") {
		t.Fatal("Redis integration tests must not run in production")
	}
	address := os.Getenv("TEST_REDIS_ADDR")
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatalf("TEST_REDIS_ADDR must be host:port: %v", err)
	}
	ip := net.ParseIP(host)
	if !strings.EqualFold(host, "localhost") && (ip == nil || !ip.IsLoopback()) {
		t.Fatal("TEST_REDIS_ADDR must be loopback")
	}
	database, err := strconv.Atoi(os.Getenv("TEST_REDIS_DB"))
	if err != nil || database < 2 || database > 15 {
		t.Fatal("TEST_REDIS_DB must be an integer between 2 and 15")
	}
	for name, fallback := range map[string]int{"CACHE_REDIS_DB": 0, "QUEUE_REDIS_DB": 1} {
		configured := fallback
		if raw := os.Getenv(name); raw != "" {
			configured, err = strconv.Atoi(raw)
			if err != nil || configured < 0 || configured > 15 {
				t.Fatalf("%s must be an integer between 0 and 15", name)
			}
		}
		if configured == database {
			t.Fatalf("TEST_REDIS_DB must differ from %s", name)
		}
	}
	password := os.Getenv("TEST_REDIS_PASSWORD")
	if password == "" {
		t.Fatal("TEST_REDIS_PASSWORD is required")
	}
	client := redis.NewClient(&redis.Options{
		Addr: address, Username: os.Getenv("TEST_REDIS_USERNAME"), Password: password, DB: database,
		DialTimeout: time.Second, ReadTimeout: time.Second, WriteTimeout: time.Second,
		PoolTimeout: time.Second, MaxRetries: -1, ContextTimeoutEnabled: true,
	})
	if err := client.Ping(t.Context()).Err(); err != nil {
		_ = client.Close()
		t.Fatalf("ping test Redis: %v", err)
	}
	if err := client.FlushDB(t.Context()).Err(); err != nil {
		_ = client.Close()
		t.Fatalf("flush test Redis: %v", err)
	}
	t.Cleanup(func() {
		_ = client.FlushDB(context.Background()).Err()
		_ = client.Close()
	})
	return client
}

func waitFor(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition did not become true before timeout")
}
