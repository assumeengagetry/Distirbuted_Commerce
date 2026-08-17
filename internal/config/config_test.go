package config

import (
	"maps"
	"strings"
	"testing"
	"time"
)

const testPasetoKey = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestLoadDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := load(mapLookup(map[string]string{
		"DATABASE_URL":        "postgres://commerce:secret@localhost:5432/commerce?sslmode=disable",
		"PASETO_V4_LOCAL_KEY": testPasetoKey,
	}))
	if err != nil {
		t.Fatalf("load() error = %v", err)
	}

	if cfg.Environment != "local" {
		t.Errorf("Environment = %q, want local", cfg.Environment)
	}
	if cfg.ServiceName != "user-service" {
		t.Errorf("ServiceName = %q, want user-service", cfg.ServiceName)
	}
	if cfg.HTTP.Address != "127.0.0.1:8081" {
		t.Errorf("HTTP.Address = %q, want 127.0.0.1:8081", cfg.HTTP.Address)
	}
	if cfg.GRPC.Address != "127.0.0.1:9091" || cfg.GRPC.IdentityTarget != "" ||
		cfg.GRPC.CallTimeout != 0 || cfg.GRPC.RequestTimeout != 2*time.Second || cfg.GRPC.ShutdownTimeout != 10*time.Second {
		t.Errorf("gRPC defaults = %+v", cfg.GRPC)
	}
	if cfg.Database.MaxConns != 20 || cfg.Database.MinIdleConns != 2 {
		t.Errorf("database pool size = (%d, %d), want (20, 2)", cfg.Database.MaxConns, cfg.Database.MinIdleConns)
	}
	if cfg.Database.PingTimeout != 2*time.Second {
		t.Errorf("Database.PingTimeout = %s, want 2s", cfg.Database.PingTimeout)
	}
	if cfg.Database.OperationTimeout != 5*time.Second || cfg.Database.LockTimeout != 2*time.Second {
		t.Errorf("database operation timeouts = (%s, %s), want (5s, 2s)", cfg.Database.OperationTimeout, cfg.Database.LockTimeout)
	}
	if cfg.Database.CommitResolutionTimeout != 0 {
		t.Errorf("Database.CommitResolutionTimeout = %s, want 0 for user-service", cfg.Database.CommitResolutionTimeout)
	}
	if cfg.Log.Level != "info" {
		t.Errorf("Log.Level = %q, want info", cfg.Log.Level)
	}
	if cfg.Auth.AccessTokenTTL != 15*time.Minute || cfg.Auth.RefreshTokenTTL != 7*24*time.Hour {
		t.Errorf("token TTLs = (%s, %s), want (15m, 168h)", cfg.Auth.AccessTokenTTL, cfg.Auth.RefreshTokenTTL)
	}
	if cfg.Commerce.RateLimit != (RateLimitConfig{}) || cfg.Payment.RateLimit != (RateLimitConfig{}) {
		t.Errorf("user-service loaded unowned rate limits: %+v %+v", cfg.Commerce, cfg.Payment)
	}
}

func TestLoadOverrides(t *testing.T) {
	t.Parallel()

	cfg, err := load(mapLookup(map[string]string{
		"APP_ENV":                      "PRODUCTION",
		"SERVICE_NAME":                 "accounts",
		"HTTP_ADDR":                    "127.0.0.1:9090",
		"HTTP_SHUTDOWN_TIMEOUT":        "25s",
		"DATABASE_URL":                 "postgres://commerce:secret@db:5432/commerce?sslmode=verify-full",
		"DB_MAX_CONNS":                 "50",
		"DB_MIN_IDLE_CONNS":            "5",
		"DB_MAX_CONN_LIFETIME":         "45m",
		"DB_MAX_CONN_LIFETIME_JITTER":  "2m",
		"DB_MAX_CONN_IDLE_TIME":        "10m",
		"DB_HEALTH_CHECK_PERIOD":       "20s",
		"DB_PING_TIMEOUT":              "3s",
		"LOG_LEVEL":                    "DEBUG",
		"HTTP_READ_HEADER_TIMEOUT":     "4s",
		"HTTP_TLS_CERT_FILE":           "/run/secrets/tls.crt",
		"HTTP_TLS_KEY_FILE":            "/run/secrets/tls.key",
		"GRPC_TLS_CERT_FILE":           "/run/secrets/grpc.crt",
		"GRPC_TLS_KEY_FILE":            "/run/secrets/grpc.key",
		"GRPC_TLS_CA_FILE":             "/run/secrets/grpc-ca.crt",
		"GRPC_TLS_ALLOWED_CLIENT_URIS": "spiffe://commerce.internal/order-service,spiffe://commerce.internal/payment-service",
		"PASETO_V4_LOCAL_KEY":          testPasetoKey,
		"ACCESS_TOKEN_TTL":             "20m",
		"REFRESH_TOKEN_TTL":            "240h",
		"AUTH_RATE_LIMIT_RPS":          "3.5",
	}))
	if err != nil {
		t.Fatalf("load() error = %v", err)
	}

	if cfg.Environment != "production" || cfg.Log.Level != "debug" {
		t.Errorf("normalized values = (%q, %q), want (production, debug)", cfg.Environment, cfg.Log.Level)
	}
	if cfg.HTTP.Address != "127.0.0.1:9090" || cfg.HTTP.ShutdownTimeout != 25*time.Second {
		t.Errorf("HTTP config = (%q, %s), want (127.0.0.1:9090, 25s)", cfg.HTTP.Address, cfg.HTTP.ShutdownTimeout)
	}
	if cfg.Database.MaxConns != 50 || cfg.Database.MinIdleConns != 5 {
		t.Errorf("database pool size = (%d, %d), want (50, 5)", cfg.Database.MaxConns, cfg.Database.MinIdleConns)
	}
}

func TestLoadUsesProcessSpecificDefaults(t *testing.T) {
	t.Parallel()
	cfg, err := loadWithDefaults(mapLookup(map[string]string{
		"DATABASE_URL": "postgres://commerce:secret@localhost:5432/commerce?sslmode=disable",
	}), "order-service", "127.0.0.1:8082")
	if err != nil {
		t.Fatalf("loadWithDefaults() error = %v", err)
	}
	if cfg.ServiceName != "order-service" || cfg.HTTP.Address != "127.0.0.1:8082" {
		t.Fatalf("process defaults = (%q, %q)", cfg.ServiceName, cfg.HTTP.Address)
	}
	if cfg.GRPC.Address != "" || cfg.GRPC.IdentityTarget != "127.0.0.1:9091" ||
		cfg.GRPC.CallTimeout != time.Second || cfg.GRPC.RequestTimeout != 0 || cfg.GRPC.ShutdownTimeout != 0 {
		t.Fatalf("order-service gRPC config = %+v", cfg.GRPC)
	}
	if cfg.Commerce.RateLimit.RequestsPerSecond != 20 || cfg.Commerce.RateLimit.Burst != 40 ||
		cfg.Auth != (AuthConfig{}) || cfg.Payment != (CommerceConfig{}) {
		t.Fatalf("order-service owned config = %+v", cfg)
	}
}

func TestLoadUsesPaymentOwnedDefaults(t *testing.T) {
	t.Parallel()
	cfg, err := loadWithDefaults(mapLookup(map[string]string{
		"DATABASE_URL": "postgres://commerce:secret@localhost:5432/commerce?sslmode=disable",
	}), "payment-service", "127.0.0.1:8083")
	if err != nil {
		t.Fatalf("loadWithDefaults() error = %v", err)
	}
	if cfg.Payment.RateLimit.RequestsPerSecond != 10 || cfg.Payment.RateLimit.Burst != 20 ||
		cfg.Auth != (AuthConfig{}) || cfg.Commerce != (CommerceConfig{}) {
		t.Fatalf("payment-service owned config = %+v", cfg)
	}
}

func TestLoadRejectsPasetoKeyOutsideIdentityService(t *testing.T) {
	t.Parallel()
	_, err := loadWithDefaults(mapLookup(map[string]string{
		"DATABASE_URL":        "postgres://commerce:secret@localhost:5432/commerce?sslmode=disable",
		"PASETO_V4_LOCAL_KEY": testPasetoKey,
	}), "payment-service", "127.0.0.1:8083")
	if err == nil || !strings.Contains(err.Error(), "must not be configured") {
		t.Fatalf("loadWithDefaults() error = %v, want PASETO isolation error", err)
	}
}

func TestLoadAllowsProductionIdentityClientWithMTLS(t *testing.T) {
	t.Parallel()
	cfg, err := loadWithDefaults(mapLookup(map[string]string{
		"APP_ENV":              "production",
		"DATABASE_URL":         "postgres://commerce@db:5432/commerce?sslmode=verify-full",
		"HTTP_TLS_CERT_FILE":   "/run/secrets/http.crt",
		"HTTP_TLS_KEY_FILE":    "/run/secrets/http.key",
		"GRPC_TLS_CERT_FILE":   "/run/secrets/client.crt",
		"GRPC_TLS_KEY_FILE":    "/run/secrets/client.key",
		"GRPC_TLS_CA_FILE":     "/run/secrets/ca.crt",
		"GRPC_TLS_SERVER_NAME": "identity.internal",
	}), "order-service", "127.0.0.1:8082")
	if err != nil {
		t.Fatalf("loadWithDefaults() error = %v", err)
	}
	if cfg.Auth.PasetoV4LocalKey != "" || cfg.GRPC.TLSServerName != "identity.internal" {
		t.Fatalf("production identity client config = %+v", cfg.GRPC)
	}
}

func TestLoadRejectsCombinedRequestBudget(t *testing.T) {
	t.Parallel()
	_, err := loadWithDefaults(mapLookup(map[string]string{
		"DATABASE_URL":                 "postgres://commerce:secret@localhost:5432/commerce?sslmode=disable",
		"HTTP_WRITE_TIMEOUT":           "15s",
		"GRPC_CALL_TIMEOUT":            "5s",
		"DB_OPERATION_TIMEOUT":         "8s",
		"DB_COMMIT_RESOLUTION_TIMEOUT": "2s",
	}), "payment-service", "127.0.0.1:8083")
	if err == nil || !strings.Contains(err.Error(), "must fit within HTTP_WRITE_TIMEOUT") {
		t.Fatalf("loadWithDefaults() error = %v, want combined request budget error", err)
	}
}

func TestLoadValidatesCommitResolutionOnlyForTransactionOwners(t *testing.T) {
	t.Parallel()
	_, err := loadWithDefaults(mapLookup(map[string]string{
		"DATABASE_URL":                 "postgres://commerce:secret@localhost:5432/commerce?sslmode=disable",
		"HTTP_WRITE_TIMEOUT":           "15s",
		"DB_OPERATION_TIMEOUT":         "10s",
		"DB_COMMIT_RESOLUTION_TIMEOUT": "5s",
	}), "order-service", "127.0.0.1:8082")
	if err == nil || !strings.Contains(err.Error(), "DB_COMMIT_RESOLUTION_TIMEOUT") {
		t.Fatalf("order-service load error = %v, want commit-resolution budget error", err)
	}
	if _, err := load(mapLookup(map[string]string{
		"DATABASE_URL":                 "postgres://commerce:secret@localhost:5432/commerce?sslmode=disable",
		"PASETO_V4_LOCAL_KEY":          testPasetoKey,
		"DB_COMMIT_RESOLUTION_TIMEOUT": "invalid",
	})); err != nil {
		t.Fatalf("user-service rejected unowned commit-resolution setting: %v", err)
	}
}

func TestLoadValidatesOnlyOwnedRateLimits(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		serviceName string
		httpAddress string
		values      map[string]string
		wantErr     string
	}{
		{name: "order commerce", serviceName: "order-service", httpAddress: "127.0.0.1:8082", values: map[string]string{"COMMERCE_RATE_LIMIT_RPS": "0", "PAYMENT_RATE_LIMIT_RPS": "invalid"}, wantErr: "COMMERCE_RATE_LIMIT_RPS"},
		{name: "payment", serviceName: "payment-service", httpAddress: "127.0.0.1:8083", values: map[string]string{"PAYMENT_RATE_LIMIT_RPS": "0", "AUTH_RATE_LIMIT_RPS": "invalid"}, wantErr: "PAYMENT_RATE_LIMIT_RPS"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			values := maps.Clone(test.values)
			values["DATABASE_URL"] = "postgres://commerce:secret@localhost:5432/commerce?sslmode=disable"
			_, err := loadWithDefaults(mapLookup(values), test.serviceName, test.httpAddress)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("loadWithDefaults() error = %v, want %s", err, test.wantErr)
			}
		})
	}
}

func TestLoadIgnoresUnownedGRPCSettings(t *testing.T) {
	t.Parallel()

	_, err := loadWithDefaults(mapLookup(map[string]string{
		"DATABASE_URL":         "postgres://commerce:secret@localhost:5432/commerce?sslmode=disable",
		"PASETO_V4_LOCAL_KEY":  testPasetoKey,
		"IDENTITY_GRPC_TARGET": "not-an-address",
		"GRPC_CALL_TIMEOUT":    "not-a-duration",
		"GRPC_TLS_SERVER_NAME": "unused",
	}), "user-service", "127.0.0.1:8081")
	if err != nil {
		t.Fatalf("user-service rejected client-only gRPC settings: %v", err)
	}

	_, err = loadWithDefaults(mapLookup(map[string]string{
		"DATABASE_URL":                 "postgres://commerce:secret@localhost:5432/commerce?sslmode=disable",
		"GRPC_ADDR":                    "not-an-address",
		"GRPC_REQUEST_TIMEOUT":         "not-a-duration",
		"GRPC_SHUTDOWN_TIMEOUT":        "not-a-duration",
		"GRPC_TLS_ALLOWED_CLIENT_URIS": "not-a-spiffe-uri",
	}), "order-service", "127.0.0.1:8082")
	if err != nil {
		t.Fatalf("order-service rejected server-only gRPC settings: %v", err)
	}
}

func TestAllowedSPIFFEURIsRejectMalformedIdentities(t *testing.T) {
	t.Parallel()
	if _, err := allowedSPIFFEURIs("spiffe://team_a/order-service"); err != nil {
		t.Fatalf("allowedSPIFFEURIs() rejected a valid underscore trust domain: %v", err)
	}
	for _, value := range []string{
		"spiffe://commerce.internal",
		"spiffe://commerce.internal/",
		"spiffe://Commerce.internal/order-service",
		"spiffe://commerce.internal:443/order-service",
		"spiffe://commerce.internal/order%2Dservice",
		"spiffe://commerce.internal/./order-service",
		"spiffe://commerce.internal/order-service/",
	} {
		value := value
		t.Run(value, func(t *testing.T) {
			t.Parallel()
			if _, err := allowedSPIFFEURIs(value); err == nil {
				t.Fatalf("allowedSPIFFEURIs(%q) error = nil", value)
			}
		})
	}
}

func TestLoadRejectsInvalidConfiguration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(map[string]string)
		wantErr string
	}{
		{
			name: "missing PASETO key",
			mutate: func(values map[string]string) {
				delete(values, "PASETO_V4_LOCAL_KEY")
			},
			wantErr: "PASETO_V4_LOCAL_KEY",
		},
		{
			name: "invalid PASETO key",
			mutate: func(values map[string]string) {
				values["PASETO_V4_LOCAL_KEY"] = "short"
			},
			wantErr: "PASETO_V4_LOCAL_KEY",
		},
		{
			name: "missing database URL",
			mutate: func(values map[string]string) {
				delete(values, "DATABASE_URL")
			},
			wantErr: "DATABASE_URL is required",
		},
		{
			name: "refresh TTL does not exceed access TTL",
			mutate: func(values map[string]string) {
				values["ACCESS_TOKEN_TTL"] = "1h"
				values["REFRESH_TOKEN_TTL"] = "1h"
			},
			wantErr: "REFRESH_TOKEN_TTL",
		},
		{
			name: "invalid Argon2 memory",
			mutate: func(values map[string]string) {
				values["ARGON2_MEMORY_KIB"] = "1024"
			},
			wantErr: "ARGON2_MEMORY_KIB",
		},
		{
			name: "invalid auth rate",
			mutate: func(values map[string]string) {
				values["AUTH_RATE_LIMIT_RPS"] = "0"
			},
			wantErr: "AUTH_RATE_LIMIT_RPS",
		},
		{
			name: "NaN auth rate",
			mutate: func(values map[string]string) {
				values["AUTH_RATE_LIMIT_RPS"] = "NaN"
			},
			wantErr: "AUTH_RATE_LIMIT_RPS",
		},
		{
			name: "incomplete TLS configuration",
			mutate: func(values map[string]string) {
				values["HTTP_TLS_CERT_FILE"] = "/tmp/cert.pem"
			},
			wantErr: "HTTP_TLS_CERT_FILE",
		},
		{
			name: "incomplete gRPC TLS configuration",
			mutate: func(values map[string]string) {
				values["GRPC_TLS_CERT_FILE"] = "/tmp/cert.pem"
			},
			wantErr: "GRPC_TLS_CERT_FILE",
		},
		{
			name: "plaintext gRPC is not loopback",
			mutate: func(values map[string]string) {
				values["GRPC_ADDR"] = "0.0.0.0:9091"
			},
			wantErr: "must be loopback",
		},
		{
			name: "plaintext HTTP is not loopback",
			mutate: func(values map[string]string) {
				values["HTTP_ADDR"] = "0.0.0.0:8081"
			},
			wantErr: "must be loopback",
		},
		{
			name: "invalid client SPIFFE URI",
			mutate: func(values map[string]string) {
				values["GRPC_TLS_ALLOWED_CLIENT_URIS"] = "https://commerce.internal/order-service"
			},
			wantErr: "canonical SPIFFE",
		},
		{
			name: "invalid environment",
			mutate: func(values map[string]string) {
				values["APP_ENV"] = "staging"
			},
			wantErr: "APP_ENV",
		},
		{
			name: "invalid log level",
			mutate: func(values map[string]string) {
				values["LOG_LEVEL"] = "verbose"
			},
			wantErr: "LOG_LEVEL",
		},
		{
			name: "invalid duration",
			mutate: func(values map[string]string) {
				values["DB_PING_TIMEOUT"] = "immediately"
			},
			wantErr: "DB_PING_TIMEOUT",
		},
		{
			name: "request work exceeds write timeout",
			mutate: func(values map[string]string) {
				values["DB_OPERATION_TIMEOUT"] = "15s"
			},
			wantErr: "request read",
		},
		{
			name: "database lock exceeds operation timeout",
			mutate: func(values map[string]string) {
				values["DB_OPERATION_TIMEOUT"] = "2s"
				values["DB_LOCK_TIMEOUT"] = "2s"
			},
			wantErr: "DB_LOCK_TIMEOUT",
		},
		{
			name: "non-positive duration",
			mutate: func(values map[string]string) {
				values["HTTP_READ_TIMEOUT"] = "0s"
			},
			wantErr: "HTTP_READ_TIMEOUT",
		},
		{
			name: "invalid integer",
			mutate: func(values map[string]string) {
				values["DB_MAX_CONNS"] = "many"
			},
			wantErr: "DB_MAX_CONNS",
		},
		{
			name: "connection limit too large",
			mutate: func(values map[string]string) {
				values["DB_MAX_CONNS"] = "10000"
			},
			wantErr: "DB_MAX_CONNS",
		},
		{
			name: "minimum exceeds maximum",
			mutate: func(values map[string]string) {
				values["DB_MIN_IDLE_CONNS"] = "21"
			},
			wantErr: "DB_MIN_IDLE_CONNS must not exceed DB_MAX_CONNS",
		},
		{
			name: "empty HTTP address",
			mutate: func(values map[string]string) {
				values["HTTP_ADDR"] = ""
			},
			wantErr: "HTTP_ADDR",
		},
		{
			name: "shutdown shorter than write timeout",
			mutate: func(values map[string]string) {
				values["HTTP_SHUTDOWN_TIMEOUT"] = "5s"
			},
			wantErr: "HTTP_SHUTDOWN_TIMEOUT",
		},
		{
			name: "jitter exceeds lifetime",
			mutate: func(values map[string]string) {
				values["DB_MAX_CONN_LIFETIME"] = "1m"
				values["DB_MAX_CONN_LIFETIME_JITTER"] = "2m"
			},
			wantErr: "DB_MAX_CONN_LIFETIME_JITTER",
		},
	}

	base := map[string]string{
		"DATABASE_URL":        "postgres://commerce:secret@localhost:5432/commerce",
		"PASETO_V4_LOCAL_KEY": testPasetoKey,
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			values := maps.Clone(base)
			tt.mutate(values)

			_, err := load(mapLookup(values))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("load() error = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadRequiresVerifiedDatabaseTLSInProduction(t *testing.T) {
	t.Parallel()

	_, err := load(mapLookup(map[string]string{
		"APP_ENV":             "production",
		"DATABASE_URL":        "postgres://commerce@db:5432/commerce?sslmode=require",
		"PASETO_V4_LOCAL_KEY": testPasetoKey,
	}))
	if err == nil || !strings.Contains(err.Error(), "sslmode=verify-full") {
		t.Fatalf("load() error = %v, want production TLS validation error", err)
	}
}

func TestLoadRequiresHTTPSTLSInProduction(t *testing.T) {
	t.Parallel()

	_, err := load(mapLookup(map[string]string{
		"APP_ENV":             "production",
		"DATABASE_URL":        "postgres://commerce@db:5432/commerce?sslmode=verify-full",
		"PASETO_V4_LOCAL_KEY": testPasetoKey,
	}))
	if err == nil || !strings.Contains(err.Error(), "HTTP TLS") {
		t.Fatalf("load() error = %v, want production HTTP TLS validation error", err)
	}
}

func TestLoadRejectsProductionDatabaseHostOverride(t *testing.T) {
	t.Parallel()
	_, err := load(mapLookup(map[string]string{
		"APP_ENV":             "production",
		"DATABASE_URL":        "postgres://commerce@db:5432/commerce?sslmode=verify-full&host=%2Fvar%2Frun%2Fpostgresql",
		"HTTP_TLS_CERT_FILE":  "/run/secrets/tls.crt",
		"HTTP_TLS_KEY_FILE":   "/run/secrets/tls.key",
		"PASETO_V4_LOCAL_KEY": testPasetoKey,
	}))
	if err == nil || !strings.Contains(err.Error(), "verified TLS targets") {
		t.Fatalf("load() error = %v, want effective database TLS validation error", err)
	}
}

func mapLookup(values map[string]string) lookupEnv {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}
