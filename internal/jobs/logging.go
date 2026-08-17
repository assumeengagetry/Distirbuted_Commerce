package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/hibiken/asynq"
)

func NewErrorHandler(logger *slog.Logger) asynq.ErrorHandler {
	logger = nonNilLogger(logger)
	return asynq.ErrorHandlerFunc(func(ctx context.Context, task *asynq.Task, err error) {
		taskType := ""
		if task != nil {
			taskType = task.Type()
		}
		taskID, _ := asynq.GetTaskID(ctx)
		queue, _ := asynq.GetQueueName(ctx)
		retried, retriedOK := asynq.GetRetryCount(ctx)
		maxRetry, maxRetryOK := asynq.GetMaxRetry(ctx)
		terminal := errors.Is(err, asynq.SkipRetry) || errors.Is(err, asynq.RevokeTask) ||
			(retriedOK && maxRetryOK && retried >= maxRetry)

		log := logger.WarnContext
		if terminal || asynq.IsPanicError(err) {
			log = logger.ErrorContext
		}
		log(ctx, "background task failed",
			slog.String("task_type", taskType),
			slog.String("task_id", taskID),
			slog.String("queue", queue),
			slog.Int("retried", retried),
			slog.Int("max_retry", maxRetry),
			slog.Bool("panic", asynq.IsPanicError(err)),
			slog.Bool("terminal", terminal),
			slog.Any("error", err),
		)
	})
}

type asynqLogger struct {
	logger *slog.Logger
}

func NewLogger(logger *slog.Logger) asynq.Logger {
	return &asynqLogger{logger: nonNilLogger(logger)}
}

func (l *asynqLogger) Debug(args ...interface{}) {
	l.logger.Debug(fmt.Sprint(args...))
}

func (l *asynqLogger) Info(args ...interface{}) {
	l.logger.Info(fmt.Sprint(args...))
}

func (l *asynqLogger) Warn(args ...interface{}) {
	l.logger.Warn(fmt.Sprint(args...))
}

func (l *asynqLogger) Error(args ...interface{}) {
	l.logger.Error(fmt.Sprint(args...))
}

func (l *asynqLogger) Fatal(args ...interface{}) {
	l.logger.Error(fmt.Sprint(args...), slog.Bool("fatal", true))
}

func nonNilLogger(logger *slog.Logger) *slog.Logger {
	if logger == nil {
		return slog.Default()
	}
	return logger
}

var _ asynq.Logger = (*asynqLogger)(nil)
