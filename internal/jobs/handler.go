package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/hibiken/asynq"
)

const sessionCleanupSchema = 1

type SessionRepository interface {
	DeleteExpiredSessions(context.Context, int32) (int64, error)
}

type HandlerConfig struct {
	DatabaseTimeout time.Duration
	TaskTimeout     time.Duration
	BatchSize       int32
}

type Handler struct {
	repository      SessionRepository
	logger          *slog.Logger
	databaseTimeout time.Duration
	taskTimeout     time.Duration
	batchSize       int32
}

func NewHandler(repository SessionRepository, logger *slog.Logger, cfg HandlerConfig) (*Handler, error) {
	if repository == nil {
		return nil, fmt.Errorf("session repository is required")
	}
	if logger == nil {
		return nil, fmt.Errorf("logger is required")
	}
	if cfg.DatabaseTimeout <= 0 {
		return nil, fmt.Errorf("database timeout must be positive")
	}
	if cfg.TaskTimeout <= cfg.DatabaseTimeout {
		return nil, fmt.Errorf("task timeout must be longer than database timeout")
	}
	if cfg.BatchSize <= 0 {
		return nil, fmt.Errorf("session cleanup batch size must be positive")
	}
	return &Handler{
		repository:      repository,
		logger:          logger,
		databaseTimeout: cfg.DatabaseTimeout,
		taskTimeout:     cfg.TaskTimeout,
		batchSize:       cfg.BatchSize,
	}, nil
}

func (h *Handler) Register(mux *asynq.ServeMux) {
	mux.Use(func(next asynq.Handler) asynq.Handler {
		return asynq.HandlerFunc(func(ctx context.Context, task *asynq.Task) error {
			if task == nil || task.Type() != SessionCleanupTaskType {
				return fmt.Errorf("unknown task type: %w", asynq.SkipRetry)
			}
			return next.ProcessTask(ctx, task)
		})
	})
	mux.HandleFunc(SessionCleanupTaskType, h.HandleSessionCleanup)
}

func (h *Handler) HandleSessionCleanup(ctx context.Context, task *asynq.Task) error {
	if task == nil || task.Type() != SessionCleanupTaskType || !validSessionCleanupPayload(task.Payload()) {
		return fmt.Errorf("invalid session cleanup payload: %w", asynq.SkipRetry)
	}

	taskCtx, taskCancel := context.WithTimeout(ctx, h.taskTimeout)
	defer taskCancel()

	var totalDeleted int64
	for {
		databaseCtx, databaseCancel := context.WithTimeout(taskCtx, h.databaseTimeout)
		deleted, err := h.repository.DeleteExpiredSessions(databaseCtx, h.batchSize)
		databaseCancel()
		if err != nil {
			return fmt.Errorf("delete expired sessions: %w", err)
		}
		if deleted < 0 || deleted > int64(h.batchSize) {
			return fmt.Errorf("delete expired sessions: unexpected row count %d", deleted)
		}
		totalDeleted += deleted
		if deleted < int64(h.batchSize) {
			break
		}
	}

	h.logger.InfoContext(ctx, "expired auth sessions deleted", slog.Int64("count", totalDeleted))
	return nil
}

func validSessionCleanupPayload(data []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(data))

	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') || !decoder.More() {
		return false
	}
	key, err := decoder.Token()
	if err != nil || key != "schema" {
		return false
	}
	var schema int
	if err := decoder.Decode(&schema); err != nil || decoder.More() {
		return false
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') {
		return false
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return false
	}
	return schema == sessionCleanupSchema
}
