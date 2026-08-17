package observability

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	otelprometheus "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/exemplar"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
	"google.golang.org/grpc/credentials"

	"github.com/assumeengagetry/distributed-commerce/internal/config"
)

type Providers struct {
	TracerProvider trace.TracerProvider
	MeterProvider  metric.MeterProvider
	Propagator     propagation.TextMapPropagator
}

func (p Providers) Enabled() bool {
	return p.TracerProvider != nil && p.MeterProvider != nil && p.Propagator != nil
}

func NoopProviders() Providers {
	return Providers{
		TracerProvider: tracenoop.NewTracerProvider(),
		MeterProvider:  metricnoop.NewMeterProvider(),
		Propagator:     propagation.TraceContext{},
	}
}

type Runtime struct {
	providers      Providers
	metricsHandler http.Handler
	meterProvider  *sdkmetric.MeterProvider
	tracerProvider *sdktrace.TracerProvider
}

func NewRuntime(
	ctx context.Context,
	cfg config.TelemetryConfig,
	serviceName, environment string,
) (*Runtime, error) {
	if ctx == nil || serviceName == "" || environment == "" {
		return nil, fmt.Errorf("telemetry context, service name, and environment are required")
	}
	if cfg.Metrics.GatherTimeout <= 0 || cfg.Metrics.WriteTimeout <= 0 {
		return nil, fmt.Errorf("telemetry metrics gather and write timeouts must be positive")
	}
	if cfg.Metrics.GatherTimeout >= cfg.Metrics.WriteTimeout {
		return nil, fmt.Errorf("telemetry metrics gather timeout must be shorter than write timeout")
	}
	if cfg.TracesExporter != "none" && cfg.TracesExporter != "otlp" {
		return nil, fmt.Errorf("telemetry trace exporter must be none or otlp")
	}
	if cfg.TracesExporter == "otlp" && cfg.ShutdownTimeout < 2*cfg.ExportTimeout+time.Second {
		return nil, fmt.Errorf("telemetry shutdown timeout does not cover the trace queue")
	}

	res, err := resource.New(
		ctx,
		resource.WithTelemetrySDK(),
		resource.WithAttributes(
			semconv.ServiceName(serviceName),
			semconv.ServiceNamespace("distributed-commerce"),
			semconv.DeploymentEnvironmentName(environment),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("create telemetry resource: %w", err)
	}
	registry := prometheus.NewRegistry()
	for _, collector := range []prometheus.Collector{
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	} {
		if err := registry.Register(collector); err != nil {
			return nil, fmt.Errorf("register runtime metric collector: %w", err)
		}
	}
	prometheusReader, err := otelprometheus.New(otelprometheus.WithRegisterer(registry))
	if err != nil {
		return nil, fmt.Errorf("create Prometheus metric reader: %w", err)
	}
	meterProvider := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(prometheusReader),
		sdkmetric.WithCardinalityLimit(256),
		sdkmetric.WithExemplarFilter(exemplar.AlwaysOffFilter),
		sdkmetric.WithView(telemetryMetricViews()...),
	)

	tracerProvider := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.NeverSample()),
	)
	var traceProvider trace.TracerProvider = tracerProvider
	if cfg.TracesExporter == "otlp" {
		exporterOptions, optionErr := traceExporterOptions(cfg)
		if optionErr != nil {
			_ = tracerProvider.Shutdown(context.Background())
			_ = meterProvider.Shutdown(context.Background())
			return nil, optionErr
		}
		exportCtx, cancel := context.WithTimeout(ctx, cfg.ExportTimeout)
		exporter, exportErr := otlptracegrpc.New(
			exportCtx,
			exporterOptions...,
		)
		cancel()
		if exportErr != nil {
			_ = tracerProvider.Shutdown(context.Background())
			_ = meterProvider.Shutdown(context.Background())
			return nil, fmt.Errorf("create OTLP trace exporter: %w", exportErr)
		}
		if err := tracerProvider.Shutdown(context.Background()); err != nil {
			_ = meterProvider.Shutdown(context.Background())
			return nil, fmt.Errorf("replace disabled trace provider: %w", err)
		}
		ratioSampler := sdktrace.TraceIDRatioBased(cfg.TraceSampleRatio)
		tracerProvider = sdktrace.NewTracerProvider(
			sdktrace.WithResource(res),
			sdktrace.WithSampler(sdktrace.ParentBased(
				ratioSampler,
				sdktrace.WithRemoteParentSampled(ratioSampler),
				sdktrace.WithRemoteParentNotSampled(ratioSampler),
			)),
			sdktrace.WithBatcher(
				exporter,
				sdktrace.WithMaxQueueSize(256),
				sdktrace.WithMaxExportBatchSize(256),
				sdktrace.WithBatchTimeout(2*time.Second),
				sdktrace.WithExportTimeout(cfg.ExportTimeout),
			),
		)
		traceProvider = tracerProvider
	}

	handler := promhttp.HandlerFor(registry, promhttp.HandlerOpts{
		EnableOpenMetrics:   true,
		MaxRequestsInFlight: 2,
		Timeout:             cfg.Metrics.GatherTimeout,
	})
	handler = promhttp.InstrumentMetricHandler(registry, handler)
	return &Runtime{
		providers: Providers{
			TracerProvider: traceProvider,
			MeterProvider:  meterProvider,
			Propagator:     propagation.TraceContext{},
		},
		metricsHandler: handler,
		meterProvider:  meterProvider,
		tracerProvider: tracerProvider,
	}, nil
}

func (r *Runtime) Providers() Providers {
	if r == nil {
		return NoopProviders()
	}
	return r.providers
}

func (r *Runtime) MetricsHandler() http.Handler {
	if r == nil || r.metricsHandler == nil {
		return http.NotFoundHandler()
	}
	return r.metricsHandler
}

func (r *Runtime) Shutdown(ctx context.Context) error {
	if r == nil {
		return nil
	}
	if ctx == nil {
		return fmt.Errorf("telemetry shutdown context is required")
	}
	var result error
	if r.tracerProvider != nil {
		result = errors.Join(result, r.tracerProvider.Shutdown(ctx))
	}
	if r.meterProvider != nil {
		result = errors.Join(result, r.meterProvider.Shutdown(ctx))
	}
	return result
}

func telemetryMetricViews() []sdkmetric.View {
	return []sdkmetric.View{
		sdkmetric.NewView(
			sdkmetric.Instrument{Name: "http.server.*"},
			sdkmetric.Stream{AttributeFilter: attribute.NewAllowKeysFilter(
				attribute.Key("http.request.method"),
				attribute.Key("http.response.status_code"),
				attribute.Key("http.route"),
				attribute.Key("network.protocol.version"),
			)},
		),
		sdkmetric.NewView(
			sdkmetric.Instrument{Name: "rpc.server.*"},
			sdkmetric.Stream{AttributeFilter: rpcMetricAttributeFilter()},
		),
		sdkmetric.NewView(
			sdkmetric.Instrument{Name: "rpc.client.*"},
			sdkmetric.Stream{AttributeFilter: rpcMetricAttributeFilter()},
		),
		sdkmetric.NewView(
			sdkmetric.Instrument{Name: "db.client.connections.*"},
			sdkmetric.Stream{AttributeFilter: attribute.NewAllowKeysFilter(
				attribute.Key("pool.name"),
				attribute.Key("state"),
				attribute.Key("type"),
				attribute.Key("status"),
				attribute.Key("error.type"),
				attribute.Key("error_type"),
			)},
		),
		sdkmetric.NewView(
			sdkmetric.Instrument{Name: "db.client.operation.*"},
			sdkmetric.Stream{AttributeFilter: attribute.NewAllowKeysFilter(
				attribute.Key("db.system.name"),
				attribute.Key("db.namespace"),
				attribute.Key("db.namespace.name"),
				attribute.Key("db.operation.name"),
				attribute.Key("pgx.operation.type"),
				attribute.Key("error.type"),
			)},
		),
		sdkmetric.NewView(
			sdkmetric.Instrument{Name: "pgxpool.*"},
			sdkmetric.Stream{AttributeFilter: attribute.NewAllowKeysFilter(
				attribute.Key("db.system.name"),
				attribute.Key("db.client.connection.pool.name"),
			)},
		),
	}
}

func rpcMetricAttributeFilter() attribute.Filter {
	return attribute.NewAllowKeysFilter(
		attribute.Key("rpc.system"),
		attribute.Key("rpc.system.name"),
		attribute.Key("rpc.service"),
		attribute.Key("rpc.method"),
		attribute.Key("rpc.grpc.status_code"),
		attribute.Key("rpc.response.status_code"),
	)
}

func traceExporterOptions(cfg config.TelemetryConfig) ([]otlptracegrpc.Option, error) {
	options := []otlptracegrpc.Option{
		otlptracegrpc.WithEndpointURL(cfg.OTLPEndpoint),
	}
	if len(cfg.OTLPHeaders) > 0 {
		options = append(options, otlptracegrpc.WithHeaders(cfg.OTLPHeaders))
	}
	endpoint, err := url.Parse(cfg.OTLPEndpoint)
	if err != nil {
		return nil, fmt.Errorf("parse OTLP trace endpoint: %w", err)
	}
	if endpoint.Scheme == "http" {
		return append(options, otlptracegrpc.WithInsecure()), nil
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS13, ServerName: endpoint.Hostname()}
	if cfg.OTLPTLSCAFile != "" {
		caPEM, err := os.ReadFile(cfg.OTLPTLSCAFile)
		if err != nil {
			return nil, fmt.Errorf("read OTLP CA: %w", err)
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(caPEM) {
			return nil, fmt.Errorf("OTLP CA contains no certificates")
		}
		tlsConfig.RootCAs = roots
	}
	if cfg.OTLPTLSCertFile != "" {
		certificate, err := tls.LoadX509KeyPair(cfg.OTLPTLSCertFile, cfg.OTLPTLSKeyFile)
		if err != nil {
			return nil, fmt.Errorf("load OTLP client certificate: %w", err)
		}
		tlsConfig.Certificates = []tls.Certificate{certificate}
	}
	return append(options, otlptracegrpc.WithTLSCredentials(credentials.NewTLS(tlsConfig))), nil
}

func (r *Runtime) ShutdownWithin(timeout time.Duration) error {
	if timeout <= 0 {
		return fmt.Errorf("telemetry shutdown timeout must be positive")
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return r.Shutdown(ctx)
}
