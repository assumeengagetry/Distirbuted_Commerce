package grpcserver

import (
	"context"
	"crypto/x509"
	"io"
	"log/slog"
	"net"
	"net/url"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"

	"github.com/assumeengagetry/distributed-commerce/internal/config"
	"github.com/assumeengagetry/distributed-commerce/internal/observability"
)

func TestDeadlineInterceptorBoundsRequest(t *testing.T) {
	t.Parallel()
	interceptor := deadlineInterceptor(20 * time.Millisecond)
	started := time.Now()
	_, err := interceptor(t.Context(), nil, &grpc.UnaryServerInfo{}, func(ctx context.Context, _ any) (any, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	if err == nil || time.Since(started) > time.Second {
		t.Fatalf("deadline interceptor error = %v, elapsed = %s", err, time.Since(started))
	}
}

func TestRecoveryInterceptorSanitizesPanic(t *testing.T) {
	t.Parallel()
	interceptor := recoveryInterceptor(slog.New(slog.NewJSONHandler(io.Discard, nil)))
	_, err := interceptor(t.Context(), nil, &grpc.UnaryServerInfo{FullMethod: "/test.Service/Method"}, func(context.Context, any) (any, error) {
		panic("private panic detail")
	})
	if status.Code(err) != codes.Internal || status.Convert(err).Message() != "internal server error" {
		t.Fatalf("recovery error = %v", err)
	}
}

func TestServeHealthAndGracefulShutdown(t *testing.T) {
	t.Parallel()
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve address: %v", err)
	}
	address := probe.Addr().String()
	if err := probe.Close(); err != nil {
		t.Fatalf("release address: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	serverErrors := make(chan error, 1)
	unavailable := make(chan struct{})
	go func() {
		serverErrors <- Serve(ctx, config.GRPCConfig{
			Address: address, RequestTimeout: time.Second, ShutdownTimeout: time.Second,
		}, slog.New(slog.NewJSONHandler(io.Discard, nil)), observability.NoopProviders(), func(grpc.ServiceRegistrar) {}, nil, func() { close(unavailable) })
	}()
	connection, err := grpc.NewClient(address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		cancel()
		t.Fatalf("grpc.NewClient() error = %v", err)
	}
	defer connection.Close()
	healthClient := healthpb.NewHealthClient(connection)
	deadline := time.Now().Add(2 * time.Second)
	for {
		checkCtx, checkCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		response, checkErr := healthClient.Check(checkCtx, &healthpb.HealthCheckRequest{})
		checkCancel()
		if checkErr == nil && response.GetStatus() == healthpb.HealthCheckResponse_SERVING {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("gRPC health did not become serving: %v", checkErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-unavailable:
	case <-time.After(time.Second):
		t.Fatal("gRPC server did not report unavailable before shutdown")
	}
	select {
	case err := <-serverErrors:
		if err != nil {
			t.Fatalf("Serve() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("gRPC server did not stop")
	}
}

func TestServeBoundsIncompleteConnectionHandshake(t *testing.T) {
	t.Parallel()
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve address: %v", err)
	}
	address := probe.Addr().String()
	if err := probe.Close(); err != nil {
		t.Fatalf("release address: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	listening := make(chan struct{})
	serverErrors := make(chan error, 1)
	go func() {
		serverErrors <- Serve(ctx, config.GRPCConfig{
			Address: address, RequestTimeout: time.Second, ShutdownTimeout: 50 * time.Millisecond,
		}, slog.New(slog.NewJSONHandler(io.Discard, nil)), observability.NoopProviders(), func(grpc.ServiceRegistrar) {}, func() { close(listening) }, nil)
	}()
	select {
	case <-listening:
	case <-time.After(time.Second):
		t.Fatal("gRPC server did not start listening")
	}
	connection, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatalf("dial incomplete handshake: %v", err)
	}
	defer connection.Close()
	if err := connection.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	buffer := make([]byte, 1024)
	for {
		if _, err := connection.Read(buffer); err != nil {
			if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
				t.Fatal("incomplete handshake connection exceeded its deadline")
			}
			break
		}
	}
	cancel()
	select {
	case err := <-serverErrors:
		if err != nil {
			t.Fatalf("Serve() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("gRPC server did not stop")
	}
}

func TestServerTransportCredentialsRejectIncompleteMTLS(t *testing.T) {
	t.Parallel()
	_, err := serverTransportCredentials(config.GRPCConfig{TLSCertFile: "cert.pem"})
	if err == nil {
		t.Fatal("serverTransportCredentials() error = nil")
	}
}

func TestClientCertificateAllowedUsesURIAllowlist(t *testing.T) {
	t.Parallel()
	identity, err := url.Parse("spiffe://commerce.internal/order-service")
	if err != nil {
		t.Fatalf("url.Parse() error = %v", err)
	}
	certificate := &x509.Certificate{URIs: []*url.URL{identity}}
	if !clientCertificateAllowed(
		[]*x509.Certificate{certificate}, []string{"spiffe://commerce.internal/order-service"},
	) {
		t.Fatal("allowed client certificate was rejected")
	}
	if clientCertificateAllowed(
		[]*x509.Certificate{certificate}, []string{"spiffe://commerce.internal/inventory-service"},
	) {
		t.Fatal("unlisted client certificate was accepted")
	}
	extraIdentity, err := url.Parse("spiffe://commerce.internal/extra-service")
	if err != nil {
		t.Fatalf("url.Parse() extra identity error = %v", err)
	}
	certificate.URIs = append(certificate.URIs, extraIdentity)
	if clientCertificateAllowed(
		[]*x509.Certificate{certificate}, []string{"spiffe://commerce.internal/order-service"},
	) {
		t.Fatal("certificate with multiple URI identities was accepted")
	}
}
