package jobs

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/hibiken/asynq"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/assumeengagetry/distributed-commerce/internal/observability"
)

func TestJobTelemetryPropagatesTraceAndRecordsMetrics(t *testing.T) {
	_, redisClient := newTestRedis(t)
	providers, spanRecorder, metricReader, shutdown := testJobProviders(t)
	t.Cleanup(shutdown)
	client, err := NewClient(redisClient, testClientConfig(), providers)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	parentCtx, parent := providers.TracerProvider.Tracer("test").Start(t.Context(), "request")
	if err := client.ScheduleSessionCleanup(parentCtx); err != nil {
		t.Fatalf("ScheduleSessionCleanup() error = %v", err)
	}
	parent.End()

	tasks, err := asynq.NewInspectorFromRedisClient(redisClient).ListScheduledTasks(testClientConfig().Queue)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("scheduled tasks = (%d, %v), want one", len(tasks), err)
	}
	if tasks[0].Headers["traceparent"] == "" {
		t.Fatal("scheduled task has no traceparent header")
	}
	repository := &stubSessionRepository{delete: func(context.Context, int32) (int64, error) { return 2, nil }}
	handler, err := NewHandler(repository, slog.New(slog.NewJSONHandler(io.Discard, nil)), testHandlerConfig(), providers)
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	mux := asynq.NewServeMux()
	handler.Register(mux)
	task := asynq.NewTaskWithHeaders(tasks[0].Type, tasks[0].Payload, tasks[0].Headers)
	if err := mux.ProcessTask(context.Background(), task); err != nil {
		t.Fatalf("ProcessTask() error = %v", err)
	}

	var producer, consumer sdktrace.ReadOnlySpan
	for _, span := range spanRecorder.Ended() {
		switch span.Name() {
		case SessionCleanupTaskType + " publish":
			producer = span
		case SessionCleanupTaskType + " process":
			consumer = span
		}
	}
	if producer == nil || consumer == nil {
		t.Fatalf("ended spans do not contain producer and consumer: %+v", spanRecorder.Ended())
	}
	if consumer.Parent().SpanID() != producer.SpanContext().SpanID() ||
		consumer.SpanContext().TraceID() != producer.SpanContext().TraceID() {
		t.Fatal("consumer span did not inherit the producer context")
	}

	var metrics metricdata.ResourceMetrics
	if err := metricReader.Collect(t.Context(), &metrics); err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	for _, name := range []string{
		"commerce.jobs.enqueues", "commerce.jobs.task.runs",
		"commerce.jobs.task.duration", "commerce.jobs.sessions.deleted",
	} {
		if !resourceMetricsContain(metrics, name) {
			t.Errorf("job metrics do not contain %q", name)
		}
	}
}

func TestJobTelemetryFinalizesConsumerSpanOnPanic(t *testing.T) {
	providers, spanRecorder, _, shutdown := testJobProviders(t)
	t.Cleanup(shutdown)
	repository := &stubSessionRepository{delete: func(context.Context, int32) (int64, error) {
		panic("repository panic")
	}}
	handler, err := NewHandler(repository, slog.New(slog.NewJSONHandler(io.Discard, nil)), testHandlerConfig(), providers)
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	mux := asynq.NewServeMux()
	handler.Register(mux)
	recovered := func() (value any) {
		defer func() { value = recover() }()
		_ = mux.ProcessTask(context.Background(), NewSessionCleanupTask())
		return nil
	}()
	if recovered == nil {
		t.Fatal("consumer middleware swallowed a panic")
	}
	for _, span := range spanRecorder.Ended() {
		if span.Name() == SessionCleanupTaskType+" process" {
			return
		}
	}
	t.Fatal("consumer span was not ended after panic")
}

func TestTaskTelemetryResultClassifiesStableOutcomes(t *testing.T) {
	t.Parallel()
	if got := taskTelemetryResult(context.Background(), nil, nil); got != "success" {
		t.Fatalf("success result = %q", got)
	}
	if got := taskTelemetryResult(context.Background(), asynq.SkipRetry, nil); got != "archive" {
		t.Fatalf("archive result = %q", got)
	}
	if got := taskTelemetryResult(context.Background(), asynq.RevokeTask, nil); got != "delete" {
		t.Fatalf("delete result = %q", got)
	}
	if got := taskTelemetryResult(context.Background(), context.DeadlineExceeded, nil); got != "retry" {
		t.Fatalf("retry result = %q", got)
	}
	if got := taskTelemetryResult(context.Background(), nil, "panic"); got != "panic" {
		t.Fatalf("panic result = %q", got)
	}
}

func testJobProviders(t *testing.T) (
	observability.Providers,
	*tracetest.SpanRecorder,
	*sdkmetric.ManualReader,
	func(),
) {
	t.Helper()
	spanRecorder := tracetest.NewSpanRecorder()
	tracerProvider := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()), sdktrace.WithSpanProcessor(spanRecorder),
	)
	metricReader := sdkmetric.NewManualReader()
	meterProvider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(metricReader))
	providers := observability.Providers{
		TracerProvider: tracerProvider, MeterProvider: meterProvider, Propagator: propagation.TraceContext{},
	}
	return providers, spanRecorder, metricReader, func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = meterProvider.Shutdown(ctx)
		_ = tracerProvider.Shutdown(ctx)
	}
}

func resourceMetricsContain(metrics metricdata.ResourceMetrics, want string) bool {
	for _, scope := range metrics.ScopeMetrics {
		for _, item := range scope.Metrics {
			if item.Name == want {
				return true
			}
		}
	}
	return false
}
