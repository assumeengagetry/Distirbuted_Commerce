package observability

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"go.opentelemetry.io/otel/trace"

	"github.com/assumeengagetry/distributed-commerce/internal/config"
)

func NewLogger(output io.Writer, cfg config.LogConfig, serviceName, environment string) (*slog.Logger, error) {
	level, err := parseLevel(cfg.Level)
	if err != nil {
		return nil, err
	}

	handler := traceContextHandler{slog.NewJSONHandler(output, &slog.HandlerOptions{Level: level})}
	return slog.New(handler).With(
		slog.String("service", serviceName),
		slog.String("environment", environment),
	), nil
}

type traceContextHandler struct {
	next slog.Handler
}

func (h traceContextHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h traceContextHandler) Handle(ctx context.Context, record slog.Record) error {
	spanContext := trace.SpanContextFromContext(ctx)
	if spanContext.IsValid() {
		record.AddAttrs(
			slog.String("trace_id", spanContext.TraceID().String()),
			slog.String("span_id", spanContext.SpanID().String()),
		)
	}
	return h.next.Handle(ctx, record)
}

func (h traceContextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return traceContextHandler{next: h.next.WithAttrs(attrs)}
}

func (h traceContextHandler) WithGroup(name string) slog.Handler {
	return traceContextHandler{next: h.next.WithGroup(name)}
}

func parseLevel(value string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("unsupported log level %q", value)
	}
}
