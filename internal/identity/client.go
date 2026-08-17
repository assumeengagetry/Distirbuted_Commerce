package identity

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"

	"github.com/assumeengagetry/distributed-commerce/internal/auth"
	identityv1 "github.com/assumeengagetry/distributed-commerce/internal/genproto/identity/v1"
)

const (
	identityServiceName        = "identity.v1.IdentityService"
	maximumClientMetadataBytes = 16 << 10
)

type ClientConfig struct {
	Target        string
	Timeout       time.Duration
	TLSCertFile   string
	TLSKeyFile    string
	TLSCAFile     string
	TLSServerName string
	Logger        *slog.Logger
}

type Client struct {
	identity identityv1.IdentityServiceClient
	health   healthpb.HealthClient
	timeout  time.Duration
	close    func() error
	logger   *slog.Logger
	target   string
}

func Dial(config ClientConfig) (*Client, error) {
	if config.Target == "" {
		return nil, fmt.Errorf("identity gRPC target is required")
	}
	transportCredentials, err := clientTransportCredentials(config)
	if err != nil {
		return nil, err
	}
	connection, err := grpc.NewClient(
		config.Target,
		grpc.WithTransportCredentials(transportCredentials),
		grpc.WithDisableRetry(),
		grpc.WithMaxHeaderListSize(maximumClientMetadataBytes),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallSendMsgSize(1<<20),
			grpc.MaxCallRecvMsgSize(4<<20),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("create identity gRPC client: %w", err)
	}
	client, err := NewClient(connection, config.Timeout)
	if err != nil {
		_ = connection.Close()
		return nil, err
	}
	client.close = connection.Close
	client.target = config.Target
	if config.Logger != nil {
		client.logger = config.Logger
	}
	return client, nil
}

func NewClient(connection grpc.ClientConnInterface, timeout time.Duration) (*Client, error) {
	if connection == nil {
		return nil, fmt.Errorf("identity gRPC connection is required")
	}
	if timeout <= 0 {
		return nil, fmt.Errorf("identity gRPC timeout must be positive")
	}
	return &Client{
		identity: identityv1.NewIdentityServiceClient(connection),
		health:   healthpb.NewHealthClient(connection),
		timeout:  timeout,
		logger:   slog.New(slog.NewJSONHandler(io.Discard, nil)),
	}, nil
}

func (c *Client) VerifyAccess(ctx context.Context, accessToken string) (auth.Principal, error) {
	if !auth.ValidAccessTokenLength(accessToken) {
		return auth.Principal{}, auth.ErrInvalidAccessToken
	}
	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	response, err := c.identity.ValidateAccessToken(callCtx, &identityv1.ValidateAccessTokenRequest{
		AccessToken: accessToken,
	})
	if err != nil {
		if status.Code(err) == codes.Unauthenticated {
			return auth.Principal{}, auth.ErrInvalidAccessToken
		}
		c.logFailure(ctx, "validate access token", err)
		return auth.Principal{}, fmt.Errorf(
			"%w: identity validation RPC failed: %w", auth.ErrAccessTokenVerifierUnavailable, err,
		)
	}
	userID, err := uuid.Parse(response.GetUserId())
	if err != nil || userID == uuid.Nil {
		return auth.Principal{}, fmt.Errorf("%w: identity returned an invalid principal", auth.ErrAccessTokenVerifierUnavailable)
	}
	tokenID, err := uuid.Parse(response.GetTokenId())
	if err != nil || tokenID == uuid.Nil {
		return auth.Principal{}, fmt.Errorf("%w: identity returned an invalid principal", auth.ErrAccessTokenVerifierUnavailable)
	}
	var role string
	switch response.GetRole() {
	case identityv1.PrincipalRole_PRINCIPAL_ROLE_CUSTOMER:
		role = "customer"
	case identityv1.PrincipalRole_PRINCIPAL_ROLE_ADMIN:
		role = "admin"
	default:
		return auth.Principal{}, fmt.Errorf("%w: identity returned an invalid principal", auth.ErrAccessTokenVerifierUnavailable)
	}
	return auth.Principal{UserID: userID, Role: role, TokenID: tokenID}, nil
}

func (c *Client) Health(ctx context.Context) error {
	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	response, err := c.health.Check(callCtx, &healthpb.HealthCheckRequest{Service: identityServiceName})
	if err != nil {
		c.logFailure(ctx, "health check", err)
		return fmt.Errorf("identity gRPC health check failed: %w", err)
	}
	if response.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		return fmt.Errorf("identity gRPC is not serving")
	}
	return nil
}

func (c *Client) logFailure(ctx context.Context, operation string, err error) {
	c.logger.WarnContext(
		ctx, "identity gRPC dependency failed",
		slog.String("operation", operation),
		slog.String("target", c.target),
		slog.String("grpc_code", status.Code(err).String()),
		slog.Any("error", err),
	)
}

func (c *Client) Close() error {
	if c.close == nil {
		return nil
	}
	return c.close()
}

func clientTransportCredentials(config ClientConfig) (credentials.TransportCredentials, error) {
	if config.TLSCAFile == "" {
		if config.TLSCertFile != "" || config.TLSKeyFile != "" || config.TLSServerName != "" {
			return nil, fmt.Errorf("identity gRPC TLS configuration is incomplete")
		}
		return insecure.NewCredentials(), nil
	}
	if config.TLSCertFile == "" || config.TLSKeyFile == "" || config.TLSServerName == "" {
		return nil, fmt.Errorf("identity gRPC mutual TLS certificate, key, CA, and server name are required")
	}
	certificate, err := tls.LoadX509KeyPair(config.TLSCertFile, config.TLSKeyFile)
	if err != nil {
		return nil, fmt.Errorf("load identity gRPC client certificate: %w", err)
	}
	caPEM, err := os.ReadFile(config.TLSCAFile)
	if err != nil {
		return nil, fmt.Errorf("read identity gRPC CA: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("identity gRPC CA contains no certificates")
	}
	return credentials.NewTLS(&tls.Config{
		MinVersion: tls.VersionTLS13, RootCAs: roots, Certificates: []tls.Certificate{certificate},
		ServerName: config.TLSServerName,
	}), nil
}
