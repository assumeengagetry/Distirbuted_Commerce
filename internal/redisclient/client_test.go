package redisclient

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/assumeengagetry/distributed-commerce/internal/config"
	"github.com/assumeengagetry/distributed-commerce/internal/observability"
)

func TestNewClientConnectsWithBoundedOptions(t *testing.T) {
	t.Parallel()
	server := miniredis.RunT(t)
	server.RequireUserAuth("commerce", "secret")
	client, err := New(testConfig(server.Addr()))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if err := client.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("Ping() error = %v", err)
	}
	options := client.Options()
	if options.MaxRetries != 0 || !options.ContextTimeoutEnabled || options.DB != 3 || options.PoolSize != 4 {
		t.Fatalf("Redis options = retries=%d context_timeout=%t db=%d pool_size=%d",
			options.MaxRetries, options.ContextTimeoutEnabled, options.DB, options.PoolSize)
	}
}

func TestNewClientRejectsInvalidConfiguration(t *testing.T) {
	t.Parallel()
	valid := testConfig("127.0.0.1:6379")
	for _, change := range []func(*config.RedisConfig){
		func(cfg *config.RedisConfig) { cfg.Address = "" },
		func(cfg *config.RedisConfig) { cfg.Username = "" },
		func(cfg *config.RedisConfig) { cfg.Password = "" },
		func(cfg *config.RedisConfig) { cfg.Database = -1 },
		func(cfg *config.RedisConfig) { cfg.DialTimeout = 0 },
		func(cfg *config.RedisConfig) { cfg.PoolSize = 0 },
		func(cfg *config.RedisConfig) { cfg.TLSServerName = "redis.internal" },
	} {
		candidate := valid
		change(&candidate)
		if client, err := New(candidate); err == nil || client != nil {
			t.Fatalf("New() = (%v, %v), want (nil, error)", client, err)
		}
	}
}

func TestNewClientVerifiesRedisTLS13Server(t *testing.T) {
	t.Parallel()
	caFile, certificate := redisTestCertificate(t, "redis.internal")
	server, err := miniredis.RunTLS(&tls.Config{
		MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificate},
	})
	if err != nil {
		t.Fatalf("miniredis.RunTLS() error = %v", err)
	}
	t.Cleanup(server.Close)
	server.RequireUserAuth("commerce", "secret")

	cfg := testConfig(server.Addr())
	cfg.TLSCAFile = caFile
	cfg.TLSServerName = "redis.internal"
	client, err := New(cfg)
	if err != nil {
		t.Fatalf("New(TLS) error = %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if err := client.Ping(t.Context()).Err(); err != nil {
		t.Fatalf("Ping(TLS) error = %v", err)
	}
	if client.Options().TLSConfig.MinVersion != tls.VersionTLS13 {
		t.Fatalf("Redis TLS minimum = %d, want TLS 1.3", client.Options().TLSConfig.MinVersion)
	}

	wrongName := cfg
	wrongName.TLSServerName = "wrong.internal"
	wrongClient, err := New(wrongName)
	if err != nil {
		t.Fatalf("New(wrong server name) error = %v", err)
	}
	t.Cleanup(func() { _ = wrongClient.Close() })
	if err := wrongClient.Ping(t.Context()).Err(); err == nil {
		t.Fatal("Ping() accepted a Redis certificate for the wrong server name")
	}
}

func TestInstrumentAddsRedisPoolMetricsAndBoundsNames(t *testing.T) {
	t.Parallel()
	server := miniredis.RunT(t)
	server.RequireUserAuth("commerce", "secret")
	client, err := New(testConfig(server.Addr()))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	reader := sdkmetric.NewManualReader()
	meterProvider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = meterProvider.Shutdown(context.Background()) })
	providers := observability.Providers{
		TracerProvider: tracenoop.NewTracerProvider(), MeterProvider: meterProvider,
		Propagator: propagation.TraceContext{},
	}
	if err := Instrument(client, providers, "cache", true); err != nil {
		t.Fatalf("Instrument() error = %v", err)
	}
	if err := client.Ping(t.Context()).Err(); err != nil {
		t.Fatalf("Ping() error = %v", err)
	}
	var metrics metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &metrics); err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if len(metrics.ScopeMetrics) == 0 {
		t.Fatal("Redis instrumentation produced no metrics")
	}
	if err := Instrument(client, providers, server.Addr(), false); err == nil {
		t.Fatal("Instrument() accepted a dynamic pool name")
	}
}

func testConfig(address string) config.RedisConfig {
	return config.RedisConfig{
		Address: address, Username: "commerce", Password: "secret", Database: 3,
		DialTimeout: time.Second, ReadTimeout: time.Second, WriteTimeout: time.Second,
		PoolTimeout: time.Second, PoolSize: 4,
	}
}

func redisTestCertificate(t *testing.T, serverName string) (string, tls.Certificate) {
	t.Helper()
	directory := t.TempDir()
	now := time.Now().UTC()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate Redis test CA key: %v", err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Redis Test CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create Redis test CA: %v", err)
	}
	caCertificate, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parse Redis test CA: %v", err)
	}
	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate Redis test server key: %v", err)
	}
	serverTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: serverName},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames: []string{serverName},
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, caCertificate, &serverKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create Redis test server certificate: %v", err)
	}
	caFile := filepath.Join(directory, "ca.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0o600); err != nil {
		t.Fatalf("write Redis test CA: %v", err)
	}
	serverCertificate, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: mustMarshalECKey(t, serverKey)}),
	)
	if err != nil {
		t.Fatalf("parse Redis test server key pair: %v", err)
	}
	return caFile, serverCertificate
}

func mustMarshalECKey(t *testing.T, key *ecdsa.PrivateKey) []byte {
	t.Helper()
	encoded, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal Redis test server key: %v", err)
	}
	return encoded
}
