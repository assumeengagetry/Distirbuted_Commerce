package httpserver

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/assumeengagetry/distributed-commerce/internal/config"
)

func TestServeCallsShutdownStartedBeforeGracefulShutdown(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var callbackCalls atomic.Int32
	err := Serve(
		ctx,
		config.HTTPConfig{
			Address: "127.0.0.1:0", ReadHeaderTimeout: time.Second,
			ReadTimeout: time.Second, WriteTimeout: time.Second,
			IdleTimeout: time.Second, ShutdownTimeout: time.Second,
		},
		slog.New(slog.NewJSONHandler(io.Discard, nil)),
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
		func() { callbackCalls.Add(1) },
	)
	if err != nil {
		t.Fatalf("Serve() error = %v", err)
	}
	if callbackCalls.Load() != 1 {
		t.Fatalf("shutdown callback calls = %d, want 1", callbackCalls.Load())
	}
}
