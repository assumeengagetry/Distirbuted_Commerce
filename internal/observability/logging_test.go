package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"go.opentelemetry.io/otel/trace"

	"github.com/assumeengagetry/distributed-commerce/internal/config"
)

func TestNewLoggerWritesStructuredContext(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	logger, err := NewLogger(&output, config.LogConfig{Level: "info"}, "user-service", "test")
	if err != nil {
		t.Fatalf("NewLogger() error = %v", err)
	}

	logger.InfoContext(context.Background(), "service started", "component", "http")

	var entry map[string]any
	if err := json.Unmarshal(output.Bytes(), &entry); err != nil {
		t.Fatalf("unmarshal log entry: %v", err)
	}

	for key, want := range map[string]string{
		"level":       "INFO",
		"msg":         "service started",
		"service":     "user-service",
		"environment": "test",
		"component":   "http",
	} {
		if got := entry[key]; got != want {
			t.Errorf("entry[%q] = %v, want %q", key, got, want)
		}
	}
	if _, ok := entry["time"]; !ok {
		t.Error("log entry does not contain time")
	}
}

func TestLoggerAddsTraceCorrelationFromContext(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	logger, err := NewLogger(&output, config.LogConfig{Level: "info"}, "order-service", "test")
	if err != nil {
		t.Fatalf("NewLogger() error = %v", err)
	}
	traceID, err := trace.TraceIDFromHex("0102030405060708090a0b0c0d0e0f10")
	if err != nil {
		t.Fatalf("TraceIDFromHex() error = %v", err)
	}
	spanID, err := trace.SpanIDFromHex("0102030405060708")
	if err != nil {
		t.Fatalf("SpanIDFromHex() error = %v", err)
	}
	ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled,
	}))
	logger.InfoContext(ctx, "correlated")

	var entry map[string]any
	if err := json.Unmarshal(output.Bytes(), &entry); err != nil {
		t.Fatalf("unmarshal log entry: %v", err)
	}
	if entry["trace_id"] != traceID.String() || entry["span_id"] != spanID.String() {
		t.Fatalf("trace correlation = trace:%v span:%v", entry["trace_id"], entry["span_id"])
	}
}

func TestNewLoggerFiltersLowerLevels(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	logger, err := NewLogger(&output, config.LogConfig{Level: "warn"}, "user-service", "test")
	if err != nil {
		t.Fatalf("NewLogger() error = %v", err)
	}

	logger.Info("filtered")
	if output.Len() != 0 {
		t.Fatalf("filtered log output = %q, want empty", output.String())
	}
}

func TestNewLoggerRejectsUnknownLevel(t *testing.T) {
	t.Parallel()

	_, err := NewLogger(&bytes.Buffer{}, config.LogConfig{Level: "trace"}, "user-service", "test")
	if err == nil {
		t.Fatal("NewLogger() error = nil, want an error")
	}
}
