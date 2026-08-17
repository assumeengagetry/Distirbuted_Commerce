package jobs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hibiken/asynq"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/assumeengagetry/distributed-commerce/internal/observability"
)

const jobsInstrumentationName = "github.com/assumeengagetry/distributed-commerce/internal/jobs"

type jobTelemetry struct {
	enabled      bool
	tracer       trace.Tracer
	propagator   propagation.TextMapPropagator
	enqueues     metric.Int64Counter
	taskRuns     metric.Int64Counter
	taskDuration metric.Float64Histogram
	deletedRows  metric.Int64Counter
}

func newJobTelemetry(providers observability.Providers) (jobTelemetry, error) {
	if !providers.Enabled() {
		providers = observability.NoopProviders()
		return jobTelemetry{
			tracer: providers.TracerProvider.Tracer(jobsInstrumentationName), propagator: providers.Propagator,
		}, nil
	}
	meter := providers.MeterProvider.Meter(jobsInstrumentationName)
	enqueues, err := meter.Int64Counter(
		"commerce.jobs.enqueues",
		metric.WithDescription("Session cleanup task enqueue attempts"),
	)
	if err != nil {
		return jobTelemetry{}, fmt.Errorf("create job enqueue counter: %w", err)
	}
	taskRuns, err := meter.Int64Counter(
		"commerce.jobs.task.runs",
		metric.WithDescription("Background task processing attempts"),
	)
	if err != nil {
		return jobTelemetry{}, fmt.Errorf("create job run counter: %w", err)
	}
	taskDuration, err := meter.Float64Histogram(
		"commerce.jobs.task.duration",
		metric.WithDescription("Background task processing duration"),
		metric.WithUnit("s"),
	)
	if err != nil {
		return jobTelemetry{}, fmt.Errorf("create job duration histogram: %w", err)
	}
	deletedRows, err := meter.Int64Counter(
		"commerce.jobs.sessions.deleted",
		metric.WithDescription("Expired authentication sessions deleted"),
	)
	if err != nil {
		return jobTelemetry{}, fmt.Errorf("create deleted session counter: %w", err)
	}
	return jobTelemetry{
		enabled: true, tracer: providers.TracerProvider.Tracer(jobsInstrumentationName),
		propagator: providers.Propagator, enqueues: enqueues, taskRuns: taskRuns,
		taskDuration: taskDuration, deletedRows: deletedRows,
	}, nil
}

func (t jobTelemetry) startProducer(ctx context.Context) (context.Context, trace.Span) {
	return t.tracer.Start(
		ctx,
		SessionCleanupTaskType+" publish",
		trace.WithSpanKind(trace.SpanKindProducer),
		trace.WithAttributes(
			attribute.String("messaging.system", "asynq"),
			attribute.String("messaging.operation.name", "publish"),
			attribute.String("messaging.message.type", SessionCleanupTaskType),
		),
	)
}

func (t jobTelemetry) sessionCleanupTask(ctx context.Context) *asynq.Task {
	if !t.enabled {
		return NewSessionCleanupTask()
	}
	headers := make(map[string]string)
	t.propagator.Inject(ctx, propagation.MapCarrier(headers))
	return asynq.NewTaskWithHeaders(SessionCleanupTaskType, []byte(sessionCleanupPayload), headers)
}

func (t jobTelemetry) recordEnqueue(ctx context.Context, result string) {
	if !t.enabled {
		return
	}
	t.enqueues.Add(ctx, 1, metric.WithAttributes(
		attribute.String("messaging.message.type", SessionCleanupTaskType),
		attribute.String("commerce.job.result", result),
	))
}

func (t jobTelemetry) middleware() asynq.MiddlewareFunc {
	return func(next asynq.Handler) asynq.Handler {
		return asynq.HandlerFunc(func(ctx context.Context, task *asynq.Task) (err error) {
			parent := t.propagator.Extract(ctx, propagation.MapCarrier(task.Headers()))
			taskCtx, span := t.tracer.Start(
				parent,
				task.Type()+" process",
				trace.WithSpanKind(trace.SpanKindConsumer),
				trace.WithAttributes(
					attribute.String("messaging.system", "asynq"),
					attribute.String("messaging.operation.name", "process"),
					attribute.String("messaging.message.type", task.Type()),
				),
			)
			startedAt := time.Now()
			defer func() {
				recovered := recover()
				result := taskTelemetryResult(taskCtx, err, recovered)
				if result != "success" {
					span.SetStatus(codes.Error, result)
				}
				t.taskRuns.Add(taskCtx, 1, metric.WithAttributes(
					attribute.String("messaging.message.type", task.Type()),
					attribute.String("commerce.job.result", result),
				))
				t.taskDuration.Record(taskCtx, time.Since(startedAt).Seconds(), metric.WithAttributes(
					attribute.String("messaging.message.type", task.Type()),
					attribute.String("commerce.job.result", result),
				))
				span.End()
				if recovered != nil {
					panic(recovered)
				}
			}()
			err = next.ProcessTask(taskCtx, task)
			return err
		})
	}
}

func taskTelemetryResult(ctx context.Context, err error, recovered any) string {
	if recovered != nil {
		return "panic"
	}
	if err == nil {
		return "success"
	}
	if errors.Is(err, asynq.RevokeTask) {
		return "delete"
	}
	if errors.Is(err, asynq.SkipRetry) {
		return "archive"
	}
	retried, retriedOK := asynq.GetRetryCount(ctx)
	maxRetry, maxRetryOK := asynq.GetMaxRetry(ctx)
	if retriedOK && maxRetryOK && retried >= maxRetry {
		return "archive"
	}
	return "retry"
}

func (t jobTelemetry) recordDeletedSessions(ctx context.Context, count int64) {
	if t.enabled && count > 0 {
		t.deletedRows.Add(ctx, count)
	}
}
