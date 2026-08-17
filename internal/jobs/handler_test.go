package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/hibiken/asynq"

	"github.com/assumeengagetry/distributed-commerce/internal/observability"
)

func TestNewHandlerValidation(t *testing.T) {
	validRepository := &stubSessionRepository{}
	validLogger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	validConfig := testHandlerConfig()

	tests := []struct {
		name       string
		repository SessionRepository
		logger     *slog.Logger
		config     HandlerConfig
	}{
		{name: "nil repository", logger: validLogger, config: validConfig},
		{name: "nil logger", repository: validRepository, config: validConfig},
		{name: "zero database timeout", repository: validRepository, logger: validLogger, config: withHandlerConfig(validConfig, func(cfg *HandlerConfig) { cfg.DatabaseTimeout = 0 })},
		{name: "negative database timeout", repository: validRepository, logger: validLogger, config: withHandlerConfig(validConfig, func(cfg *HandlerConfig) { cfg.DatabaseTimeout = -time.Second })},
		{name: "zero batch size", repository: validRepository, logger: validLogger, config: withHandlerConfig(validConfig, func(cfg *HandlerConfig) { cfg.BatchSize = 0 })},
		{name: "negative batch size", repository: validRepository, logger: validLogger, config: withHandlerConfig(validConfig, func(cfg *HandlerConfig) { cfg.BatchSize = -1 })},
		{name: "short task timeout", repository: validRepository, logger: validLogger, config: withHandlerConfig(validConfig, func(cfg *HandlerConfig) { cfg.TaskTimeout = cfg.DatabaseTimeout })},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if handler, err := NewHandler(test.repository, test.logger, test.config, observability.NoopProviders()); err == nil || handler != nil {
				t.Fatalf("NewHandler() = (%v, %v), want (nil, error)", handler, err)
			}
		})
	}

	if handler, err := NewHandler(validRepository, validLogger, validConfig, observability.NoopProviders()); err != nil || handler == nil {
		t.Fatalf("NewHandler() = (%v, %v), want a handler", handler, err)
	}
}

func TestHandleSessionCleanupStrictPayload(t *testing.T) {
	tests := []struct {
		name    string
		payload []byte
	}{
		{name: "empty"},
		{name: "malformed", payload: []byte(`{"schema":`)},
		{name: "non-object", payload: []byte(`[]`)},
		{name: "null", payload: []byte(`null`)},
		{name: "missing schema", payload: []byte(`{}`)},
		{name: "unknown field", payload: []byte(`{"schema":1,"other":true}`)},
		{name: "unknown field first", payload: []byte(`{"other":true,"schema":1}`)},
		{name: "duplicate schema", payload: []byte(`{"schema":1,"schema":1}`)},
		{name: "trailing object", payload: []byte(`{"schema":1} {}`)},
		{name: "unsupported schema", payload: []byte(`{"schema":2}`)},
		{name: "non-integer schema", payload: []byte(`{"schema":1.0}`)},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			called := false
			repository := &stubSessionRepository{delete: func(context.Context, int32) (int64, error) {
				called = true
				return 0, nil
			}}
			handler := mustTestHandler(t, repository, io.Discard)

			err := handler.HandleSessionCleanup(context.Background(), asynq.NewTask(SessionCleanupTaskType, test.payload))
			if !errors.Is(err, asynq.SkipRetry) {
				t.Fatalf("HandleSessionCleanup() error = %v, want SkipRetry", err)
			}
			if called {
				t.Fatal("repository was called for an invalid payload")
			}
		})
	}

	handler := mustTestHandler(t, &stubSessionRepository{}, io.Discard)
	if err := handler.HandleSessionCleanup(context.Background(), nil); !errors.Is(err, asynq.SkipRetry) {
		t.Fatalf("HandleSessionCleanup(nil) error = %v, want SkipRetry", err)
	}
	if err := handler.HandleSessionCleanup(
		context.Background(), asynq.NewTask("unknown", []byte(sessionCleanupPayload)),
	); !errors.Is(err, asynq.SkipRetry) {
		t.Fatalf("HandleSessionCleanup(unknown type) error = %v, want SkipRetry", err)
	}
}

func TestHandleSessionCleanupDatabaseErrorIsRetryable(t *testing.T) {
	databaseError := errors.New("database unavailable")
	repository := &stubSessionRepository{delete: func(ctx context.Context, _ int32) (int64, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("repository context has no deadline")
		}
		return 0, databaseError
	}}
	handler := mustTestHandler(t, repository, io.Discard)

	err := handler.HandleSessionCleanup(context.Background(), asynq.NewTask(SessionCleanupTaskType, []byte(sessionCleanupPayload)))
	if !errors.Is(err, databaseError) {
		t.Fatalf("HandleSessionCleanup() error = %v, want database error", err)
	}
	if errors.Is(err, asynq.SkipRetry) {
		t.Fatalf("HandleSessionCleanup() error = %v, must remain retryable", err)
	}
}

func TestHandleSessionCleanupUsesBatchAndLogsCount(t *testing.T) {
	var output bytes.Buffer
	repository := &stubSessionRepository{delete: func(ctx context.Context, batchSize int32) (int64, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("repository context has no deadline")
		}
		if batchSize != 250 {
			t.Errorf("batch size = %d, want 250", batchSize)
		}
		return 7, nil
	}}
	config := testHandlerConfig()
	config.BatchSize = 250
	handler, err := NewHandler(repository, slog.New(slog.NewJSONHandler(&output, nil)), config, observability.NoopProviders())
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}

	task := asynq.NewTask(SessionCleanupTaskType, []byte(sessionCleanupPayload))
	if err := handler.HandleSessionCleanup(context.Background(), task); err != nil {
		t.Fatalf("HandleSessionCleanup() error = %v", err)
	}

	var entry map[string]any
	if err := json.Unmarshal(output.Bytes(), &entry); err != nil {
		t.Fatalf("unmarshal log entry: %v", err)
	}
	if entry["count"] != float64(7) {
		t.Errorf("logged count = %v, want 7", entry["count"])
	}
	for _, forbidden := range []string{"payload", "schema", "before", "batch_size"} {
		if _, ok := entry[forbidden]; ok {
			t.Errorf("log entry contains forbidden field %q", forbidden)
		}
	}
}

func TestHandleSessionCleanupDrainsFullBatches(t *testing.T) {
	var output bytes.Buffer
	results := []int64{100, 100, 7}
	var calls int
	repository := &stubSessionRepository{delete: func(_ context.Context, batchSize int32) (int64, error) {
		if batchSize != 100 {
			t.Fatalf("batch size = %d, want 100", batchSize)
		}
		result := results[calls]
		calls++
		return result, nil
	}}
	handler := mustTestHandler(t, repository, &output)

	if err := handler.HandleSessionCleanup(
		context.Background(), asynq.NewTask(SessionCleanupTaskType, []byte(sessionCleanupPayload)),
	); err != nil {
		t.Fatalf("HandleSessionCleanup() error = %v", err)
	}
	if calls != len(results) {
		t.Fatalf("repository calls = %d, want %d", calls, len(results))
	}
	var entry map[string]any
	if err := json.Unmarshal(output.Bytes(), &entry); err != nil {
		t.Fatalf("unmarshal log entry: %v", err)
	}
	if entry["count"] != float64(207) {
		t.Fatalf("logged count = %v, want 207", entry["count"])
	}
}

func TestHandlerRegister(t *testing.T) {
	called := false
	repository := &stubSessionRepository{delete: func(context.Context, int32) (int64, error) {
		called = true
		return 0, nil
	}}
	handler := mustTestHandler(t, repository, io.Discard)
	mux := asynq.NewServeMux()
	handler.Register(mux)

	if err := mux.ProcessTask(context.Background(), asynq.NewTask(SessionCleanupTaskType, []byte(sessionCleanupPayload))); err != nil {
		t.Fatalf("ProcessTask() error = %v", err)
	}
	if !called {
		t.Fatal("registered handler did not call repository")
	}
	if err := mux.ProcessTask(context.Background(), asynq.NewTask("unknown", nil)); !errors.Is(err, asynq.SkipRetry) {
		t.Fatalf("ProcessTask(unknown) error = %v, want SkipRetry", err)
	}
}

type stubSessionRepository struct {
	delete func(context.Context, int32) (int64, error)
}

func (repository *stubSessionRepository) DeleteExpiredSessions(ctx context.Context, batchSize int32) (int64, error) {
	if repository.delete == nil {
		return 0, nil
	}
	return repository.delete(ctx, batchSize)
}

func mustTestHandler(t *testing.T, repository SessionRepository, output io.Writer) *Handler {
	t.Helper()
	handler, err := NewHandler(repository, slog.New(slog.NewJSONHandler(output, nil)), testHandlerConfig(), observability.NoopProviders())
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	return handler
}

func testHandlerConfig() HandlerConfig {
	return HandlerConfig{
		DatabaseTimeout: time.Second,
		TaskTimeout:     2 * time.Second,
		BatchSize:       100,
	}
}

func withHandlerConfig(cfg HandlerConfig, change func(*HandlerConfig)) HandlerConfig {
	change(&cfg)
	return cfg
}
