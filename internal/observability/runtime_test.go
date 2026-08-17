package observability

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	collectortracev1 "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"

	"github.com/assumeengagetry/distributed-commerce/internal/config"
)

func TestRuntimeExposesIsolatedPrometheusMetrics(t *testing.T) {
	t.Parallel()
	runtime, err := NewRuntime(context.Background(), testTelemetryConfig(), "test-service", "test")
	if err != nil {
		t.Fatalf("NewRuntime() error = %v", err)
	}
	t.Cleanup(func() {
		if err := runtime.ShutdownWithin(time.Second); err != nil {
			t.Errorf("ShutdownWithin() error = %v", err)
		}
	})
	counter, err := runtime.Providers().MeterProvider.Meter("test").Int64Counter(
		"commerce.test.operations", metric.WithDescription("test operations"),
	)
	if err != nil {
		t.Fatalf("create counter: %v", err)
	}
	counter.Add(context.Background(), 2)
	_, localSpan := runtime.Providers().TracerProvider.Tracer("test").Start(context.Background(), "local-only")
	if !localSpan.SpanContext().IsValid() || localSpan.SpanContext().IsSampled() {
		t.Fatalf("local-only span context = %v", localSpan.SpanContext())
	}
	localSpan.End()
	histogram, err := runtime.Providers().MeterProvider.Meter("test").Float64Histogram("http.server.request.duration")
	if err != nil {
		t.Fatalf("create HTTP histogram: %v", err)
	}
	histogram.Record(context.Background(), 0.1, metric.WithAttributes(
		attribute.String("http.route", "/healthz"),
		attribute.Int("server.port", 65535),
	))
	rpcHistogram, err := runtime.Providers().MeterProvider.Meter("test").Float64Histogram("rpc.server.call.duration")
	if err != nil {
		t.Fatalf("create RPC histogram: %v", err)
	}
	rpcHistogram.Record(context.Background(), 0.1, metric.WithAttributes(
		attribute.String("rpc.service", "identity.v1.IdentityService"),
		attribute.String("rpc.method", "ValidateAccessToken"),
		attribute.String("rpc.response.status_code", "OK"),
		attribute.String("server.address", "attacker.invalid"),
	))
	connections, err := runtime.Providers().MeterProvider.Meter("test").Int64UpDownCounter("db.client.connections.usage")
	if err != nil {
		t.Fatalf("create connection counter: %v", err)
	}
	connections.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("pool.name", "cache"), attribute.String("state", "idle"),
		attribute.String("server.address", "redis.internal"),
	))
	connections.Add(context.Background(), 2, metric.WithAttributes(
		attribute.String("pool.name", "cache"), attribute.String("state", "used"),
		attribute.String("server.address", "redis.internal"),
	))

	request := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	response := httptest.NewRecorder()
	runtime.MetricsHandler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("metrics status = %d", response.Code)
	}
	body := response.Body.String()
	for _, metricName := range []string{"go_goroutines", "process_cpu_seconds", "commerce_test_operations_total", "target_info"} {
		if !strings.Contains(body, metricName) {
			t.Errorf("metrics output does not contain %q", metricName)
		}
	}
	if !strings.Contains(body, `service_name="test-service"`) {
		t.Error("target info does not contain the service name")
	}
	if strings.Contains(body, "server_port") || strings.Contains(body, "65535") ||
		strings.Contains(body, "server_address") || strings.Contains(body, "attacker.invalid") ||
		strings.Contains(body, "redis.internal") {
		t.Fatal("telemetry metric retained a network address")
	}
	for _, label := range []string{`rpc_response_status_code="OK"`, `pool_name="cache"`, `state="idle"`, `state="used"`} {
		if !strings.Contains(body, label) {
			t.Errorf("metrics output does not preserve %s", label)
		}
	}
}

func TestRuntimeValidatesRequiredInputs(t *testing.T) {
	t.Parallel()
	if runtime, err := NewRuntime(nil, testTelemetryConfig(), "service", "test"); err == nil || runtime != nil {
		t.Fatalf("NewRuntime(nil context) = (%v, %v)", runtime, err)
	}
	invalid := testTelemetryConfig()
	invalid.Metrics.WriteTimeout = 0
	if runtime, err := NewRuntime(context.Background(), invalid, "service", "test"); err == nil || runtime != nil {
		t.Fatalf("NewRuntime(invalid metrics) = (%v, %v)", runtime, err)
	}
}

func TestRuntimeConstructsOptionalOTLPExporterWithoutGlobalState(t *testing.T) {
	t.Parallel()
	cfg := testTelemetryConfig()
	cfg.TracesExporter = "otlp"
	cfg.OTLPEndpoint = "http://127.0.0.1:4317"
	cfg.TraceSampleRatio = 0
	runtime, err := NewRuntime(context.Background(), cfg, "test-service", "test")
	if err != nil {
		t.Fatalf("NewRuntime(OTLP) error = %v", err)
	}
	_, span := runtime.Providers().TracerProvider.Tracer("test").Start(context.Background(), "not-sampled")
	if !span.SpanContext().IsValid() || span.SpanContext().IsSampled() {
		t.Fatalf("ratio-zero root span context = %v", span.SpanContext())
	}
	span.End()
	traceID, err := trace.TraceIDFromHex("0102030405060708090a0b0c0d0e0f10")
	if err != nil {
		t.Fatalf("TraceIDFromHex() error = %v", err)
	}
	spanID, err := trace.SpanIDFromHex("0102030405060708")
	if err != nil {
		t.Fatalf("SpanIDFromHex() error = %v", err)
	}
	remote := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled, Remote: true,
	})
	remoteCtx := trace.ContextWithRemoteSpanContext(context.Background(), remote)
	_, child := runtime.Providers().TracerProvider.Tracer("test").Start(remoteCtx, "remote-sampled")
	if child.SpanContext().IsSampled() {
		t.Fatal("remote sampled flag bypassed the local ratio-zero sampler")
	}
	child.End()
	if err := runtime.ShutdownWithin(time.Second); err != nil {
		t.Fatalf("ShutdownWithin() error = %v", err)
	}
}

func TestRuntimeRejectsUnreadableExplicitOTLPCA(t *testing.T) {
	t.Parallel()
	cfg := testTelemetryConfig()
	cfg.TracesExporter = "otlp"
	cfg.OTLPEndpoint = "https://collector.internal:4317"
	cfg.OTLPTLSCAFile = "/nonexistent/otel-ca.pem"
	if runtime, err := NewRuntime(context.Background(), cfg, "test-service", "test"); err == nil || runtime != nil {
		t.Fatalf("NewRuntime(unreadable CA) = (%v, %v)", runtime, err)
	}
}

func TestRuntimeExportsSpanToOTLPGRPCCollector(t *testing.T) {
	t.Parallel()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for test collector: %v", err)
	}
	exports := make(chan *collectortracev1.ExportTraceServiceRequest, 1)
	collector := grpc.NewServer()
	collectortracev1.RegisterTraceServiceServer(collector, &testTraceCollector{exports: exports})
	serveErrors := make(chan error, 1)
	go func() { serveErrors <- collector.Serve(listener) }()
	t.Cleanup(func() {
		collector.Stop()
		if err := <-serveErrors; err != nil && err != grpc.ErrServerStopped {
			t.Errorf("test collector Serve() error = %v", err)
		}
	})

	cfg := testTelemetryConfig()
	cfg.TracesExporter = "otlp"
	cfg.OTLPEndpoint = "http://" + listener.Addr().String()
	cfg.TraceSampleRatio = 1
	runtime, err := NewRuntime(context.Background(), cfg, "export-test", "test")
	if err != nil {
		t.Fatalf("NewRuntime() error = %v", err)
	}
	_, span := runtime.Providers().TracerProvider.Tracer("test").Start(context.Background(), "exported-span")
	span.End()
	if err := runtime.ShutdownWithin(2 * time.Second); err != nil {
		t.Fatalf("ShutdownWithin() error = %v", err)
	}
	select {
	case request := <-exports:
		if len(request.GetResourceSpans()) == 0 {
			t.Fatal("OTLP export contains no resource spans")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("test Collector did not receive an OTLP export")
	}
}

type testTraceCollector struct {
	collectortracev1.UnimplementedTraceServiceServer
	exports chan<- *collectortracev1.ExportTraceServiceRequest
}

func (c *testTraceCollector) Export(
	_ context.Context,
	request *collectortracev1.ExportTraceServiceRequest,
) (*collectortracev1.ExportTraceServiceResponse, error) {
	c.exports <- request
	return &collectortracev1.ExportTraceServiceResponse{}, nil
}

func testTelemetryConfig() config.TelemetryConfig {
	return config.TelemetryConfig{
		Metrics:        config.MetricsConfig{GatherTimeout: 500 * time.Millisecond, WriteTimeout: time.Second},
		TracesExporter: "none", ExportTimeout: time.Second, ShutdownTimeout: 3 * time.Second,
	}
}
