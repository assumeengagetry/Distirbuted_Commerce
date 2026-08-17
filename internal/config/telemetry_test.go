package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoadTelemetryDefaultsUseProcessSpecificLoopbackPorts(t *testing.T) {
	t.Parallel()
	for serviceName, wantAddress := range map[string]string{
		"user-service":    "127.0.0.1:9101",
		"order-service":   "127.0.0.1:9102",
		"payment-service": "127.0.0.1:9103",
		"job-worker":      "127.0.0.1:9104",
	} {
		serviceName, wantAddress := serviceName, wantAddress
		t.Run(serviceName, func(t *testing.T) {
			t.Parallel()
			cfg, err := loadTelemetryConfig(mapLookup(nil), "local", serviceName)
			if err != nil {
				t.Fatalf("loadTelemetryConfig() error = %v", err)
			}
			if cfg.Metrics.Address != wantAddress || cfg.TracesExporter != "none" {
				t.Fatalf("telemetry defaults = %+v, want address %s and disabled traces", cfg, wantAddress)
			}
			if cfg.ExportTimeout != 0 || cfg.ShutdownTimeout != 12*time.Second {
				t.Fatalf("telemetry timeouts = (%s, %s)", cfg.ExportTimeout, cfg.ShutdownTimeout)
			}
		})
	}
}

func TestLoadTelemetryOTLPConfiguration(t *testing.T) {
	t.Parallel()
	cfg, err := loadTelemetryConfig(mapLookup(map[string]string{
		"METRICS_ADDR":                       "localhost:9200",
		"OTEL_TRACES_EXPORTER":               "OTLP",
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "https://collector.internal:4317",
		"OTEL_TRACE_SAMPLE_RATIO":            "0.25",
		"TELEMETRY_EXPORT_TIMEOUT":           "3s",
		"TELEMETRY_SHUTDOWN_TIMEOUT":         "7s",
		"OTEL_EXPORTER_OTLP_HEADERS":         "authorization=Bearer%20token,x-tenant=commerce",
	}), "production", "user-service")
	if err != nil {
		t.Fatalf("loadTelemetryConfig() error = %v", err)
	}
	if cfg.TracesExporter != "otlp" || cfg.TraceSampleRatio != 0.25 ||
		cfg.OTLPEndpoint != "https://collector.internal:4317" || cfg.OTLPHeaders["x-tenant"] != "commerce" {
		t.Fatalf("OTLP config = %+v", cfg)
	}
}

func TestLoadTelemetryRejectsUnsafeOrInvalidConfiguration(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		environment string
		values      map[string]string
		want        string
	}{
		{name: "metrics not loopback", environment: "local", values: map[string]string{"METRICS_ADDR": "0.0.0.0:9101"}, want: "loopback"},
		{name: "unknown exporter", environment: "local", values: map[string]string{"OTEL_TRACES_EXPORTER": "zipkin"}, want: "OTEL_TRACES_EXPORTER"},
		{name: "missing endpoint", environment: "local", values: map[string]string{"OTEL_TRACES_EXPORTER": "otlp"}, want: "OTEL_EXPORTER_OTLP_ENDPOINT"},
		{name: "plaintext remote endpoint", environment: "local", values: map[string]string{"OTEL_TRACES_EXPORTER": "otlp", "OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector.internal:4317"}, want: "loopback"},
		{name: "plaintext production endpoint", environment: "production", values: map[string]string{"OTEL_TRACES_EXPORTER": "otlp", "OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4317"}, want: "https"},
		{name: "endpoint credentials", environment: "local", values: map[string]string{"OTEL_TRACES_EXPORTER": "otlp", "OTEL_EXPORTER_OTLP_ENDPOINT": "https://user:secret@collector.internal:4317"}, want: "without credentials"},
		{name: "endpoint path", environment: "local", values: map[string]string{"OTEL_TRACES_EXPORTER": "otlp", "OTEL_EXPORTER_OTLP_ENDPOINT": "https://collector.internal:4317/v1/traces"}, want: "must not contain a path"},
		{name: "invalid sample ratio", environment: "local", values: map[string]string{"OTEL_TRACES_EXPORTER": "otlp", "OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4317", "OTEL_TRACE_SAMPLE_RATIO": "NaN"}, want: "OTEL_TRACE_SAMPLE_RATIO"},
		{name: "short shutdown", environment: "local", values: map[string]string{"OTEL_TRACES_EXPORTER": "otlp", "OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4317", "TELEMETRY_EXPORT_TIMEOUT": "5s", "TELEMETRY_SHUTDOWN_TIMEOUT": "10s"}, want: "TELEMETRY_SHUTDOWN_TIMEOUT"},
		{name: "slow gather", environment: "local", values: map[string]string{"METRICS_GATHER_TIMEOUT": "5s", "METRICS_WRITE_TIMEOUT": "5s"}, want: "METRICS_GATHER_TIMEOUT"},
		{name: "short metrics shutdown", environment: "local", values: map[string]string{"METRICS_WRITE_TIMEOUT": "10s", "METRICS_SHUTDOWN_TIMEOUT": "5s"}, want: "METRICS_SHUTDOWN_TIMEOUT"},
		{name: "malformed headers", environment: "local", values: map[string]string{"OTEL_TRACES_EXPORTER": "otlp", "OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4317", "OTEL_EXPORTER_OTLP_HEADERS": "authorization"}, want: "key=value"},
		{name: "TLS with plaintext", environment: "local", values: map[string]string{"OTEL_TRACES_EXPORTER": "otlp", "OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4317", "OTEL_EXPORTER_OTLP_CERTIFICATE": "ca.pem"}, want: "https endpoint"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := loadTelemetryConfig(mapLookup(test.values), test.environment, "user-service")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("loadTelemetryConfig() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestDisabledTracingIgnoresExporterOnlySettings(t *testing.T) {
	t.Parallel()
	if _, err := loadTelemetryConfig(mapLookup(map[string]string{
		"OTEL_TRACES_EXPORTER":        "none",
		"OTEL_EXPORTER_OTLP_ENDPOINT": "not-a-url",
		"OTEL_TRACE_SAMPLE_RATIO":     "invalid",
		"TELEMETRY_EXPORT_TIMEOUT":    "invalid",
	}), "local", "user-service"); err != nil {
		t.Fatalf("disabled tracing rejected exporter settings: %v", err)
	}
}
