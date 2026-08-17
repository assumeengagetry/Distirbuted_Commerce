package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/hibiken/asynq"
)

func TestErrorHandlerLogsMetadataWithoutPayload(t *testing.T) {
	const secret = "payload-secret-that-must-not-be-logged"
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	handler := NewErrorHandler(logger)
	task := asynq.NewTask(SessionCleanupTaskType, []byte(`{"schema":1,"secret":"`+secret+`"}`))

	handler.HandleError(context.Background(), task, fmtSkipRetryError())

	if strings.Contains(output.String(), secret) || strings.Contains(output.String(), "\"payload\"") {
		t.Fatalf("error log contains task payload: %s", output.String())
	}
	var entry map[string]any
	if err := json.Unmarshal(output.Bytes(), &entry); err != nil {
		t.Fatalf("unmarshal log entry: %v", err)
	}
	if entry["task_type"] != SessionCleanupTaskType {
		t.Errorf("task_type = %v, want %q", entry["task_type"], SessionCleanupTaskType)
	}
	for key, want := range map[string]any{
		"task_id":   "",
		"queue":     "",
		"retried":   float64(0),
		"max_retry": float64(0),
		"panic":     false,
		"terminal":  true,
	} {
		if got := entry[key]; got != want {
			t.Errorf("%s = %v, want %v", key, got, want)
		}
	}
}

func TestErrorHandlerDoesNotMarkUnknownRetryStateTerminal(t *testing.T) {
	var output bytes.Buffer
	handler := NewErrorHandler(slog.New(slog.NewJSONHandler(&output, nil)))
	handler.HandleError(context.Background(), asynq.NewTask("ordinary", nil), errors.New("temporary"))

	var entry map[string]any
	if err := json.Unmarshal(output.Bytes(), &entry); err != nil {
		t.Fatalf("unmarshal log entry: %v", err)
	}
	if terminal, ok := entry["terminal"].(bool); !ok || terminal {
		t.Errorf("terminal = %v, want false", entry["terminal"])
	}
	if entry["level"] != "WARN" {
		t.Errorf("level = %v, want WARN", entry["level"])
	}
}

func TestLoggerAdaptsLevelsAndFatalReturns(t *testing.T) {
	var output bytes.Buffer
	logger := NewLogger(slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug})))

	logger.Debug("debug ", 1)
	logger.Info("info")
	logger.Warn("warn")
	logger.Error("error")
	logger.Fatal("fatal")

	lines := bytes.Split(bytes.TrimSpace(output.Bytes()), []byte("\n"))
	if len(lines) != 5 {
		t.Fatalf("log line count = %d, want 5", len(lines))
	}
	wantLevels := []string{"DEBUG", "INFO", "WARN", "ERROR", "ERROR"}
	wantMessages := []string{"debug 1", "info", "warn", "error", "fatal"}
	for index, line := range lines {
		var entry map[string]any
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatalf("unmarshal log line %d: %v", index, err)
		}
		if entry["level"] != wantLevels[index] {
			t.Errorf("line %d level = %v, want %q", index, entry["level"], wantLevels[index])
		}
		if entry["msg"] != wantMessages[index] {
			t.Errorf("line %d msg = %v, want %q", index, entry["msg"], wantMessages[index])
		}
	}

	var fatalEntry map[string]any
	if err := json.Unmarshal(lines[4], &fatalEntry); err != nil {
		t.Fatalf("unmarshal fatal log line: %v", err)
	}
	if fatalEntry["fatal"] != true {
		t.Errorf("fatal marker = %v, want true", fatalEntry["fatal"])
	}
}

func fmtSkipRetryError() error {
	return errors.Join(errors.New("invalid task"), asynq.SkipRetry)
}
