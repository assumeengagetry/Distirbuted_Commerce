package identity

import (
	"errors"
	"net"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"

	"github.com/assumeengagetry/distributed-commerce/internal/auth"
	identityv1 "github.com/assumeengagetry/distributed-commerce/internal/genproto/identity/v1"
)

func TestClientDetectsOutageAndRecoversAfterServerRestart(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve address: %v", err)
	}
	address := probe.Addr().String()
	if err := probe.Close(); err != nil {
		t.Fatalf("release address: %v", err)
	}
	response := &identityv1.ValidateAccessTokenResponse{
		UserId: uuid.NewString(), Role: identityv1.PrincipalRole_PRINCIPAL_ROLE_CUSTOMER,
		TokenId: uuid.NewString(),
	}
	stopFirst := startRestartableIdentityServer(t, address, response)
	client, err := Dial(ClientConfig{Target: address, Timeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if _, err := client.VerifyAccess(t.Context(), "token"); err != nil {
		t.Fatalf("initial VerifyAccess() error = %v", err)
	}
	stopFirst()
	if _, err := client.VerifyAccess(t.Context(), "token"); !errors.Is(err, auth.ErrAccessTokenVerifierUnavailable) {
		t.Fatalf("outage VerifyAccess() error = %v, want verifier unavailable", err)
	}
	stopSecond := startRestartableIdentityServer(t, address, response)
	t.Cleanup(stopSecond)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := client.VerifyAccess(t.Context(), "token"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("identity client did not recover after server restart")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func startRestartableIdentityServer(
	t *testing.T,
	address string,
	response *identityv1.ValidateAccessTokenResponse,
) func() {
	t.Helper()
	listener, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatalf("listen on %s: %v", address, err)
	}
	server := grpc.NewServer()
	identityv1.RegisterIdentityServiceServer(server, &stubIdentityRPC{response: response})
	serverErrors := make(chan error, 1)
	go func() { serverErrors <- server.Serve(listener) }()
	return func() {
		server.Stop()
		if err := <-serverErrors; err != nil {
			t.Errorf("identity test server error = %v", err)
		}
	}
}
