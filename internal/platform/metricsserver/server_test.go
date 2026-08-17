package metricsserver

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/assumeengagetry/distributed-commerce/internal/config"
)

func TestServeExposesOnlyMetricsAndShutsDown(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	addresses := make(chan net.Addr, 1)
	errors := make(chan error, 1)
	go func() {
		errors <- Serve(ctx, testMetricsConfig("127.0.0.1:0"), discardMetricsLogger(), http.HandlerFunc(
			func(writer http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(writer, "test_metric 1\n")
			},
		), func(address net.Addr) { addresses <- address })
	}()
	address := <-addresses

	response, err := http.Get("http://" + address.String() + "/metrics")
	if err != nil {
		cancel()
		t.Fatalf("GET /metrics error = %v", err)
	}
	body, readErr := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if readErr != nil || response.StatusCode != http.StatusOK || !strings.Contains(string(body), "test_metric 1") {
		t.Fatalf("GET /metrics = status:%d body:%q error:%v", response.StatusCode, body, readErr)
	}
	if response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("metrics security headers = %v", response.Header)
	}
	other, err := http.Get("http://" + address.String() + "/healthz")
	if err != nil {
		cancel()
		t.Fatalf("GET /healthz error = %v", err)
	}
	_ = other.Body.Close()
	if other.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /healthz status = %d, want 404", other.StatusCode)
	}

	cancel()
	select {
	case err := <-errors:
		if err != nil {
			t.Fatalf("Serve() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("metrics server did not stop")
	}
}

func TestServeRejectsNonLoopbackAddress(t *testing.T) {
	t.Parallel()
	err := Serve(context.Background(), testMetricsConfig("0.0.0.0:9101"), discardMetricsLogger(), http.NotFoundHandler(), nil)
	if err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("Serve() error = %v, want loopback error", err)
	}
}

func testMetricsConfig(address string) config.MetricsConfig {
	return config.MetricsConfig{
		Address: address, ReadHeaderTimeout: time.Second, WriteTimeout: time.Second,
		IdleTimeout: time.Second, ShutdownTimeout: time.Second,
	}
}

func discardMetricsLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}
