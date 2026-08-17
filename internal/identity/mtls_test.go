package identity

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"

	"github.com/assumeengagetry/distributed-commerce/internal/auth"
	"github.com/assumeengagetry/distributed-commerce/internal/config"
	identityv1 "github.com/assumeengagetry/distributed-commerce/internal/genproto/identity/v1"
	"github.com/assumeengagetry/distributed-commerce/internal/observability"
	"github.com/assumeengagetry/distributed-commerce/internal/platform/grpcserver"
)

func TestMutualTLSAuthenticatesServerAndAllowedClient(t *testing.T) {
	t.Parallel()
	pki := newTestPKI(t)
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
	listening := make(chan struct{})
	userID, tokenID := uuid.New(), uuid.New()
	go func() {
		serverErrors <- grpcserver.Serve(ctx, config.GRPCConfig{
			Address: address, RequestTimeout: time.Second, ShutdownTimeout: time.Second,
			TLSCertFile: pki.serverCert, TLSKeyFile: pki.serverKey, TLSCAFile: pki.caCert,
			TLSAllowedClientURIs: []string{pki.allowedURI},
		}, slog.New(slog.NewJSONHandler(io.Discard, nil)), observability.NoopProviders(), func(registrar grpc.ServiceRegistrar) {
			identityv1.RegisterIdentityServiceServer(registrar, &stubIdentityRPC{response: &identityv1.ValidateAccessTokenResponse{
				UserId: userID.String(), Role: identityv1.PrincipalRole_PRINCIPAL_ROLE_CUSTOMER,
				TokenId: tokenID.String(),
			}})
		}, func() { close(listening) }, nil)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-serverErrors:
			if err != nil {
				t.Errorf("grpcserver.Serve() error = %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("mTLS gRPC server did not stop")
		}
	})
	select {
	case <-listening:
	case <-time.After(2 * time.Second):
		t.Fatal("mTLS gRPC server did not start listening")
	}

	allowedClient, err := Dial(ClientConfig{
		Target: address, Timeout: time.Second,
		TLSCertFile: pki.allowedClientCert, TLSKeyFile: pki.allowedClientKey,
		TLSCAFile: pki.caCert, TLSServerName: pki.serverName,
	})
	if err != nil {
		t.Fatalf("Dial(allowed) error = %v", err)
	}
	t.Cleanup(func() { _ = allowedClient.Close() })
	principal, err := allowedClient.VerifyAccess(t.Context(), "token")
	if err != nil {
		t.Fatalf("VerifyAccess(allowed) error = %v", err)
	}
	if principal.UserID != userID || principal.TokenID != tokenID {
		t.Fatalf("principal = %+v", principal)
	}

	for _, test := range []struct {
		name       string
		certFile   string
		keyFile    string
		serverName string
	}{
		{name: "unlisted client", certFile: pki.unlistedClientCert, keyFile: pki.unlistedClientKey, serverName: pki.serverName},
		{name: "wrong server name", certFile: pki.allowedClientCert, keyFile: pki.allowedClientKey, serverName: "wrong.identity.test"},
	} {
		t.Run(test.name, func(t *testing.T) {
			client, err := Dial(ClientConfig{
				Target: address, Timeout: 200 * time.Millisecond,
				TLSCertFile: test.certFile, TLSKeyFile: test.keyFile,
				TLSCAFile: pki.caCert, TLSServerName: test.serverName,
			})
			if err != nil {
				t.Fatalf("Dial() error = %v", err)
			}
			defer client.Close()
			_, err = client.VerifyAccess(t.Context(), "token")
			if !errors.Is(err, auth.ErrAccessTokenVerifierUnavailable) {
				t.Fatalf("VerifyAccess() error = %v, want verifier unavailable", err)
			}
		})
	}
}

type testPKI struct {
	caCert                 string
	serverCert, serverKey  string
	allowedClientCert      string
	allowedClientKey       string
	unlistedClientCert     string
	unlistedClientKey      string
	serverName, allowedURI string
}

func newTestPKI(t *testing.T) testPKI {
	t.Helper()
	directory := t.TempDir()
	now := time.Now().UTC()
	caKey := newTestPrivateKey(t)
	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Phase 5 Test CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create CA certificate: %v", err)
	}
	caCertificate, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parse CA certificate: %v", err)
	}
	caPath := writeCertificate(t, directory, "ca.pem", caDER)
	serverName := "identity.test"
	serverCert, serverKey := issueTestCertificate(
		t, directory, "server", caCertificate, caKey,
		[]string{serverName}, nil, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, big.NewInt(2),
	)
	allowedURI := "spiffe://commerce.internal/order-service"
	allowedURL, err := url.Parse(allowedURI)
	if err != nil {
		t.Fatalf("parse allowed URI: %v", err)
	}
	allowedCert, allowedKey := issueTestCertificate(
		t, directory, "allowed-client", caCertificate, caKey,
		nil, []*url.URL{allowedURL}, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, big.NewInt(3),
	)
	unlistedURL, err := url.Parse("spiffe://commerce.internal/other-service")
	if err != nil {
		t.Fatalf("parse unlisted URI: %v", err)
	}
	unlistedCert, unlistedKey := issueTestCertificate(
		t, directory, "unlisted-client", caCertificate, caKey,
		nil, []*url.URL{unlistedURL}, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, big.NewInt(4),
	)
	return testPKI{
		caCert: caPath, serverCert: serverCert, serverKey: serverKey,
		allowedClientCert: allowedCert, allowedClientKey: allowedKey,
		unlistedClientCert: unlistedCert, unlistedClientKey: unlistedKey,
		serverName: serverName, allowedURI: allowedURI,
	}
}

func issueTestCertificate(
	t *testing.T,
	directory, name string,
	ca *x509.Certificate,
	caKey *ecdsa.PrivateKey,
	dnsNames []string,
	uriNames []*url.URL,
	usages []x509.ExtKeyUsage,
	serial *big.Int,
) (string, string) {
	t.Helper()
	key := newTestPrivateKey(t)
	template := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: usages,
		DNSNames: dnsNames, URIs: uriNames,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create %s certificate: %v", name, err)
	}
	return writeCertificate(t, directory, name+".pem", der), writePrivateKey(t, directory, name+".key", key)
}

func newTestPrivateKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate private key: %v", err)
	}
	return key
}

func writeCertificate(t *testing.T, directory, name string, der []byte) string {
	t.Helper()
	path := filepath.Join(directory, name)
	data := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write certificate: %v", err)
	}
	return path
}

func writePrivateKey(t *testing.T, directory, name string, key *ecdsa.PrivateKey) string {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal private key: %v", err)
	}
	path := filepath.Join(directory, name)
	data := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write private key: %v", err)
	}
	return path
}
