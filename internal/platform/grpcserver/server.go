package grpcserver

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"runtime/debug"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"

	"github.com/assumeengagetry/distributed-commerce/internal/config"
)

const (
	maximumRequestBytes  = 1 << 20
	maximumResponseBytes = 4 << 20
	maximumMetadataBytes = 16 << 10
)

type RegisterServices func(grpc.ServiceRegistrar)

func Serve(
	ctx context.Context,
	cfg config.GRPCConfig,
	logger *slog.Logger,
	register RegisterServices,
	onListening func(),
	onUnavailable func(),
) error {
	if ctx == nil || logger == nil || register == nil {
		return fmt.Errorf("gRPC server context, logger, and service registration are required")
	}
	if cfg.Address == "" || cfg.RequestTimeout <= 0 || cfg.ShutdownTimeout <= 0 {
		return fmt.Errorf("gRPC address and positive request/shutdown timeouts are required")
	}
	connectionTimeout := min(cfg.RequestTimeout, cfg.ShutdownTimeout)
	serverOptions := []grpc.ServerOption{
		grpc.ConnectionTimeout(connectionTimeout),
		grpc.MaxRecvMsgSize(maximumRequestBytes),
		grpc.MaxSendMsgSize(maximumResponseBytes),
		grpc.MaxHeaderListSize(maximumMetadataBytes),
		grpc.MaxConcurrentStreams(1000),
		grpc.ChainUnaryInterceptor(recoveryInterceptor(logger), deadlineInterceptor(cfg.RequestTimeout)),
	}
	tlsCredentials, err := serverTransportCredentials(cfg)
	if err != nil {
		return err
	}
	if tlsCredentials != nil {
		serverOptions = append(serverOptions, grpc.Creds(tlsCredentials))
	}
	server := grpc.NewServer(serverOptions...)
	register(server)
	healthServer := health.NewServer()
	healthpb.RegisterHealthServer(server, healthServer)
	for service := range server.GetServiceInfo() {
		healthServer.SetServingStatus(service, healthpb.HealthCheckResponse_SERVING)
	}
	healthServer.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)

	listener, err := net.Listen("tcp", cfg.Address)
	if err != nil {
		return fmt.Errorf("listen for gRPC on %s: %w", cfg.Address, err)
	}
	if onListening != nil {
		onListening()
	}
	serverErrors := make(chan error, 1)
	go func() {
		logger.InfoContext(
			ctx, "gRPC server listening",
			slog.String("address", listener.Addr().String()),
			slog.Bool("tls", tlsCredentials != nil),
		)
		serverErrors <- server.Serve(listener)
	}()

	var serveErr error
	serveResultReceived := false
	select {
	case serveErr = <-serverErrors:
		serveResultReceived = true
	case <-ctx.Done():
	}
	if onUnavailable != nil {
		onUnavailable()
	}
	healthServer.Shutdown()

	stopped := make(chan struct{})
	go func() {
		server.GracefulStop()
		close(stopped)
	}()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	select {
	case <-stopped:
	case <-shutdownCtx.Done():
		server.Stop()
		<-stopped
	}
	if !serveResultReceived {
		serveErr = <-serverErrors
	}
	if serveErr != nil && !errors.Is(serveErr, grpc.ErrServerStopped) {
		return fmt.Errorf("serve gRPC: %w", serveErr)
	}
	logger.Info("gRPC server stopped")
	return nil
}

func deadlineInterceptor(maximum time.Duration) grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		request any,
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (any, error) {
		deadline, exists := ctx.Deadline()
		if exists && time.Until(deadline) <= maximum {
			return handler(ctx, request)
		}
		boundedCtx, cancel := context.WithTimeout(ctx, maximum)
		defer cancel()
		return handler(boundedCtx, request)
	}
}

func recoveryInterceptor(logger *slog.Logger) grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		request any,
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (response any, err error) {
		defer func() {
			if recovered := recover(); recovered != nil {
				logger.ErrorContext(
					ctx, "gRPC panic recovered",
					slog.String("method", info.FullMethod),
					slog.String("stack", string(debug.Stack())),
				)
				response = nil
				err = status.Error(codes.Internal, "internal server error")
			}
		}()
		return handler(ctx, request)
	}
}

func serverTransportCredentials(cfg config.GRPCConfig) (credentials.TransportCredentials, error) {
	configured := cfg.TLSCertFile != "" || cfg.TLSKeyFile != "" || cfg.TLSCAFile != ""
	if !configured {
		return nil, nil
	}
	if cfg.TLSCertFile == "" || cfg.TLSKeyFile == "" || cfg.TLSCAFile == "" {
		return nil, fmt.Errorf("gRPC server mutual TLS certificate, key, and CA are required")
	}
	certificate, err := tls.LoadX509KeyPair(cfg.TLSCertFile, cfg.TLSKeyFile)
	if err != nil {
		return nil, fmt.Errorf("load gRPC server certificate: %w", err)
	}
	caPEM, err := os.ReadFile(cfg.TLSCAFile)
	if err != nil {
		return nil, fmt.Errorf("read gRPC client CA: %w", err)
	}
	clientRoots := x509.NewCertPool()
	if !clientRoots.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("gRPC client CA contains no certificates")
	}
	return credentials.NewTLS(&tls.Config{
		MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificate},
		ClientCAs: clientRoots, ClientAuth: tls.RequireAndVerifyClientCert,
		VerifyConnection: func(state tls.ConnectionState) error {
			if !clientCertificateAllowed(state.PeerCertificates, cfg.TLSAllowedClientURIs) {
				return errors.New("gRPC client certificate URI SAN is not allowed")
			}
			return nil
		},
	}), nil
}

func clientCertificateAllowed(certificates []*x509.Certificate, allowedURIs []string) bool {
	if len(certificates) == 0 || len(allowedURIs) == 0 {
		return false
	}
	allowed := make(map[string]struct{}, len(allowedURIs))
	for _, uri := range allowedURIs {
		allowed[uri] = struct{}{}
	}
	if len(certificates[0].URIs) != 1 {
		return false
	}
	_, ok := allowed[certificates[0].URIs[0].String()]
	return ok
}
