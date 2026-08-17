package httptransport

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/assumeengagetry/distributed-commerce/internal/observability"
)

func TestBaseRouterRecordsHTTPServerTelemetry(t *testing.T) {
	spanRecorder := tracetest.NewSpanRecorder()
	tracerProvider := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()), sdktrace.WithSpanProcessor(spanRecorder),
	)
	metricReader := sdkmetric.NewManualReader()
	meterProvider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(metricReader))
	t.Cleanup(func() {
		_ = meterProvider.Shutdown(context.Background())
		_ = tracerProvider.Shutdown(context.Background())
	})
	providers := observability.Providers{
		TracerProvider: tracerProvider, MeterProvider: meterProvider, Propagator: propagation.TraceContext{},
	}
	router, err := newBaseRouter(baseDependencies{
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)), ServiceName: "test-service",
		ReadinessChecks:  map[string]func(context.Context) error{"test": func(context.Context) error { return nil }},
		ReadinessTimeout: time.Second, Telemetry: providers,
	})
	if err != nil {
		t.Fatalf("newBaseRouter() error = %v", err)
	}
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	request.Header.Set("traceparent", "00-0102030405060708090a0b0c0d0e0f10-0102030405060708-01")
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("GET /healthz status = %d", response.Code)
	}
	if len(spanRecorder.Ended()) != 1 {
		t.Fatalf("HTTP ended spans = %d, want 1", len(spanRecorder.Ended()))
	}
	if spanRecorder.Ended()[0].SpanContext().TraceID().String() == "0102030405060708090a0b0c0d0e0f10" {
		t.Fatal("HTTP server trusted a client-controlled trace ID")
	}
	var metrics metricdata.ResourceMetrics
	if err := metricReader.Collect(t.Context(), &metrics); err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	found := false
	for _, scope := range metrics.ScopeMetrics {
		for _, item := range scope.Metrics {
			if item.Name == "http.server.request.duration" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("HTTP server duration metric was not recorded")
	}
}
