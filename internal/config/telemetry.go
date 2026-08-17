package config

import (
	"fmt"
	"math"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type MetricsConfig struct {
	Address           string
	ReadHeaderTimeout time.Duration
	GatherTimeout     time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	ShutdownTimeout   time.Duration
}

type TelemetryConfig struct {
	Metrics          MetricsConfig
	TracesExporter   string
	OTLPEndpoint     string
	OTLPTLSCAFile    string
	OTLPTLSCertFile  string
	OTLPTLSKeyFile   string
	OTLPHeaders      map[string]string
	TraceSampleRatio float64
	ExportTimeout    time.Duration
	ShutdownTimeout  time.Duration
}

func loadTelemetryConfig(
	lookup lookupEnv,
	environment, defaultServiceName string,
) (TelemetryConfig, error) {
	metricsAddress := valueOrDefault(lookup, "METRICS_ADDR", defaultMetricsAddress(defaultServiceName))
	if err := validateNetworkAddress("METRICS_ADDR", metricsAddress); err != nil {
		return TelemetryConfig{}, err
	}
	if !isLoopbackAddress(metricsAddress) {
		return TelemetryConfig{}, fmt.Errorf("METRICS_ADDR must be loopback")
	}
	readHeaderTimeout, err := boundedDuration(
		lookup, "METRICS_READ_HEADER_TIMEOUT", 2*time.Second, 100*time.Millisecond, 10*time.Second,
	)
	if err != nil {
		return TelemetryConfig{}, err
	}
	gatherTimeout, err := boundedDuration(
		lookup, "METRICS_GATHER_TIMEOUT", 3*time.Second, 100*time.Millisecond, 15*time.Second,
	)
	if err != nil {
		return TelemetryConfig{}, err
	}
	writeTimeout, err := boundedDuration(
		lookup, "METRICS_WRITE_TIMEOUT", 5*time.Second, time.Second, 30*time.Second,
	)
	if err != nil {
		return TelemetryConfig{}, err
	}
	idleTimeout, err := boundedDuration(
		lookup, "METRICS_IDLE_TIMEOUT", 30*time.Second, time.Second, 5*time.Minute,
	)
	if err != nil {
		return TelemetryConfig{}, err
	}
	metricsShutdownTimeout, err := boundedDuration(
		lookup, "METRICS_SHUTDOWN_TIMEOUT", 10*time.Second, time.Second, 30*time.Second,
	)
	if err != nil {
		return TelemetryConfig{}, err
	}
	if gatherTimeout >= writeTimeout {
		return TelemetryConfig{}, fmt.Errorf("METRICS_GATHER_TIMEOUT must be shorter than METRICS_WRITE_TIMEOUT")
	}
	if writeTimeout > metricsShutdownTimeout {
		return TelemetryConfig{}, fmt.Errorf("METRICS_SHUTDOWN_TIMEOUT must not be shorter than METRICS_WRITE_TIMEOUT")
	}

	exporter := strings.ToLower(valueOrDefault(lookup, "OTEL_TRACES_EXPORTER", "none"))
	if exporter != "none" && exporter != "otlp" {
		return TelemetryConfig{}, fmt.Errorf("OTEL_TRACES_EXPORTER must be none or otlp")
	}
	shutdownTimeout, err := boundedDuration(
		lookup, "TELEMETRY_SHUTDOWN_TIMEOUT", 12*time.Second, time.Second, 2*time.Minute,
	)
	if err != nil {
		return TelemetryConfig{}, err
	}
	config := TelemetryConfig{
		Metrics: MetricsConfig{
			Address: metricsAddress, ReadHeaderTimeout: readHeaderTimeout, GatherTimeout: gatherTimeout,
			WriteTimeout: writeTimeout, IdleTimeout: idleTimeout, ShutdownTimeout: metricsShutdownTimeout,
		},
		TracesExporter: exporter, ShutdownTimeout: shutdownTimeout,
	}
	if exporter == "none" {
		return config, nil
	}
	exportTimeout, err := boundedDuration(
		lookup, "TELEMETRY_EXPORT_TIMEOUT", 5*time.Second, 100*time.Millisecond, 30*time.Second,
	)
	if err != nil {
		return TelemetryConfig{}, err
	}
	if shutdownTimeout < 2*exportTimeout+time.Second {
		return TelemetryConfig{}, fmt.Errorf("TELEMETRY_SHUTDOWN_TIMEOUT must cover two exports plus one second")
	}
	config.ExportTimeout = exportTimeout

	endpoint := valueOrDefault(lookup, "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	if endpoint == "" {
		endpoint = valueOrDefault(lookup, "OTEL_EXPORTER_OTLP_ENDPOINT", "")
	}
	if err := validateOTLPEndpoint(endpoint, environment); err != nil {
		return TelemetryConfig{}, err
	}
	tlsCAFile := telemetryValue(
		lookup, "OTEL_EXPORTER_OTLP_TRACES_CERTIFICATE", "OTEL_EXPORTER_OTLP_CERTIFICATE",
	)
	tlsCertFile := telemetryValue(
		lookup, "OTEL_EXPORTER_OTLP_TRACES_CLIENT_CERTIFICATE", "OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE",
	)
	tlsKeyFile := telemetryValue(
		lookup, "OTEL_EXPORTER_OTLP_TRACES_CLIENT_KEY", "OTEL_EXPORTER_OTLP_CLIENT_KEY",
	)
	if (tlsCertFile == "") != (tlsKeyFile == "") {
		return TelemetryConfig{}, fmt.Errorf("OTEL exporter client certificate and key must be configured together")
	}
	if strings.HasPrefix(endpoint, "http://") && (tlsCAFile != "" || tlsCertFile != "" || tlsKeyFile != "") {
		return TelemetryConfig{}, fmt.Errorf("OTEL exporter TLS files require an https endpoint")
	}
	headers, err := telemetryHeaders(telemetryValue(
		lookup, "OTEL_EXPORTER_OTLP_TRACES_HEADERS", "OTEL_EXPORTER_OTLP_HEADERS",
	))
	if err != nil {
		return TelemetryConfig{}, err
	}
	sampleRatio, err := telemetryRatio(lookup, "OTEL_TRACE_SAMPLE_RATIO", 0.1)
	if err != nil {
		return TelemetryConfig{}, err
	}
	config.OTLPEndpoint = endpoint
	config.OTLPTLSCAFile = tlsCAFile
	config.OTLPTLSCertFile = tlsCertFile
	config.OTLPTLSKeyFile = tlsKeyFile
	config.OTLPHeaders = headers
	config.TraceSampleRatio = sampleRatio
	return config, nil
}

func defaultMetricsAddress(serviceName string) string {
	switch serviceName {
	case "user-service":
		return "127.0.0.1:9101"
	case "order-service":
		return "127.0.0.1:9102"
	case "payment-service":
		return "127.0.0.1:9103"
	case "job-worker":
		return "127.0.0.1:9104"
	default:
		return "127.0.0.1:9199"
	}
}

func validateOTLPEndpoint(raw, environment string) error {
	if raw == "" {
		return fmt.Errorf("OTEL_EXPORTER_OTLP_ENDPOINT is required when OTEL_TRACES_EXPORTER=otlp")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("OTEL exporter endpoint must be an HTTP(S) URL without credentials, query, or fragment")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("OTEL exporter endpoint must use http or https")
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return fmt.Errorf("OTLP/gRPC exporter endpoint must not contain a path")
	}
	if port := parsed.Port(); port != "" {
		portNumber, err := strconv.Atoi(port)
		if err != nil || portNumber < 1 || portNumber > 65535 {
			return fmt.Errorf("OTEL exporter endpoint port is invalid")
		}
	}
	if environment == "production" && parsed.Scheme != "https" {
		return fmt.Errorf("OTEL exporter endpoint must use https in production")
	}
	if parsed.Scheme == "http" {
		host := parsed.Hostname()
		ip := net.ParseIP(host)
		if !strings.EqualFold(host, "localhost") && (ip == nil || !ip.IsLoopback()) {
			return fmt.Errorf("plaintext OTEL exporter endpoint must be loopback")
		}
	}
	return nil
}

func telemetryValue(lookup lookupEnv, specificKey, generalKey string) string {
	if value, ok := lookup(specificKey); ok {
		return strings.TrimSpace(value)
	}
	return valueOrDefault(lookup, generalKey, "")
}

func telemetryHeaders(raw string) (map[string]string, error) {
	if raw == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	if len(parts) > 32 {
		return nil, fmt.Errorf("OTEL exporter headers must contain at most 32 values")
	}
	headers := make(map[string]string, len(parts))
	for _, part := range parts {
		key, value, ok := strings.Cut(part, "=")
		if !ok {
			return nil, fmt.Errorf("OTEL exporter headers must use key=value entries")
		}
		key, err := url.QueryUnescape(strings.TrimSpace(key))
		if err != nil {
			return nil, fmt.Errorf("OTEL exporter header name is invalid")
		}
		value, err = url.QueryUnescape(strings.TrimSpace(value))
		if err != nil {
			return nil, fmt.Errorf("OTEL exporter header value is invalid")
		}
		key = strings.ToLower(key)
		if !validTelemetryHeaderName(key) || len(value) > 4096 || strings.ContainsAny(value, "\x00\r\n") {
			return nil, fmt.Errorf("OTEL exporter header is invalid")
		}
		if _, duplicate := headers[key]; duplicate {
			return nil, fmt.Errorf("OTEL exporter headers must not contain duplicate names")
		}
		headers[key] = value
	}
	return headers, nil
}

func validTelemetryHeaderName(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for index := range len(value) {
		character := value[index]
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' ||
			character == '-' || character == '_' || character == '.' {
			continue
		}
		return false
	}
	return true
}

func telemetryRatio(lookup lookupEnv, key string, fallback float64) (float64, error) {
	raw, ok := lookup(key)
	if !ok {
		return fallback, nil
	}
	value, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 1 {
		return 0, fmt.Errorf("%s must be a number between 0 and 1", key)
	}
	return value, nil
}
