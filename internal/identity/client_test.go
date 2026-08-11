package identity

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/assumeengagetry/distributed-commerce/internal/auth"
	identityv1 "github.com/assumeengagetry/distributed-commerce/internal/genproto/identity/v1"
)

func TestClientVerifyAccessAndHealth(t *testing.T) {
	t.Parallel()
	userID, tokenID := uuid.New(), uuid.New()
	connection := startIdentityTestServer(t, &stubIdentityRPC{response: &identityv1.ValidateAccessTokenResponse{
		UserId: userID.String(), Role: identityv1.PrincipalRole_PRINCIPAL_ROLE_ADMIN, TokenId: tokenID.String(),
	}}, true)
	client, err := NewClient(connection, time.Second)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	principal, err := client.VerifyAccess(t.Context(), "access-token")
	if err != nil {
		t.Fatalf("VerifyAccess() error = %v", err)
	}
	if principal.UserID != userID || principal.TokenID != tokenID || principal.Role != "admin" {
		t.Fatalf("principal = %+v", principal)
	}
	if err := client.Health(t.Context()); err != nil {
		t.Fatalf("Health() error = %v", err)
	}
}

func TestClientMapsIdentityFailures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		server  *stubIdentityRPC
		timeout time.Duration
		want    error
	}{
		{name: "unauthenticated", server: &stubIdentityRPC{err: status.Error(codes.Unauthenticated, "private")}, timeout: time.Second, want: auth.ErrInvalidAccessToken},
		{name: "malformed principal", server: &stubIdentityRPC{response: &identityv1.ValidateAccessTokenResponse{UserId: "invalid", TokenId: uuid.NewString(), Role: identityv1.PrincipalRole_PRINCIPAL_ROLE_CUSTOMER}}, timeout: time.Second, want: auth.ErrAccessTokenVerifierUnavailable},
		{name: "deadline", server: &stubIdentityRPC{waitForCancellation: true}, timeout: 20 * time.Millisecond, want: auth.ErrAccessTokenVerifierUnavailable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			connection := startIdentityTestServer(t, test.server, false)
			client, err := NewClient(connection, test.timeout)
			if err != nil {
				t.Fatalf("NewClient() error = %v", err)
			}
			_, err = client.VerifyAccess(t.Context(), "never-log-this-token")
			if !errors.Is(err, test.want) {
				t.Fatalf("VerifyAccess() error = %v, want %v", err, test.want)
			}
			if strings.Contains(err.Error(), "never-log-this-token") {
				t.Fatalf("VerifyAccess() exposed bearer token: %v", err)
			}
		})
	}
}

func TestClientRejectsOversizedTokenLocally(t *testing.T) {
	t.Parallel()
	connection := startIdentityTestServer(t, &stubIdentityRPC{}, false)
	client, err := NewClient(connection, time.Second)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	_, err = client.VerifyAccess(t.Context(), strings.Repeat("x", 4097))
	if !errors.Is(err, auth.ErrInvalidAccessToken) {
		t.Fatalf("VerifyAccess() error = %v, want ErrInvalidAccessToken", err)
	}
}

type stubIdentityRPC struct {
	identityv1.UnimplementedIdentityServiceServer
	response            *identityv1.ValidateAccessTokenResponse
	err                 error
	waitForCancellation bool
}

func (s *stubIdentityRPC) ValidateAccessToken(
	ctx context.Context,
	_ *identityv1.ValidateAccessTokenRequest,
) (*identityv1.ValidateAccessTokenResponse, error) {
	if s.waitForCancellation {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return s.response, s.err
}

func startIdentityTestServer(
	t *testing.T,
	identityServer identityv1.IdentityServiceServer,
	withHealth bool,
) *grpc.ClientConn {
	t.Helper()
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	identityv1.RegisterIdentityServiceServer(server, identityServer)
	if withHealth {
		healthServer := health.NewServer()
		healthpb.RegisterHealthServer(server, healthServer)
		healthServer.SetServingStatus(identityServiceName, healthpb.HealthCheckResponse_SERVING)
	}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
	})
	connection, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
		grpc.WithDisableRetry(),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient() error = %v", err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	return connection
}
