package observability

import (
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/assumeengagetry/distributed-commerce/internal/config"
)

func NewLogger(output io.Writer, cfg config.LogConfig, serviceName, environment string) (*slog.Logger, error) {
	level, err := parseLevel(cfg.Level)
	if err != nil {
		return nil, err
	}

	handler := slog.NewJSONHandler(output, &slog.HandlerOptions{Level: level})
	return slog.New(handler).With(
		slog.String("service", serviceName),
		slog.String("environment", environment),
	), nil
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
