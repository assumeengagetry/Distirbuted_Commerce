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
	if cfg.Database.MaxConns != 20 || cfg.Database.MinIdleConns != 2 {
		t.Errorf("database pool size = (%d, %d), want (20, 2)", cfg.Database.MaxConns, cfg.Database.MinIdleConns)
	}
	if cfg.Database.PingTimeout != 2*time.Second {
		t.Errorf("Database.PingTimeout = %s, want 2s", cfg.Database.PingTimeout)
	}
	if cfg.Database.OperationTimeout != 5*time.Second || cfg.Database.LockTimeout != 2*time.Second {
		t.Errorf("database operation timeouts = (%s, %s), want (5s, 2s)", cfg.Database.OperationTimeout, cfg.Database.LockTimeout)
	}
	if cfg.Log.Level != "info" {
		t.Errorf("Log.Level = %q, want info", cfg.Log.Level)
	}
	if cfg.Auth.AccessTokenTTL != 15*time.Minute || cfg.Auth.RefreshTokenTTL != 7*24*time.Hour {
		t.Errorf("token TTLs = (%s, %s), want (15m, 168h)", cfg.Auth.AccessTokenTTL, cfg.Auth.RefreshTokenTTL)
	}
	if cfg.Commerce.RateLimit.RequestsPerSecond != 20 || cfg.Commerce.RateLimit.Burst != 40 {
		t.Errorf("commerce rate limit = (%v, %d), want (20, 40)", cfg.Commerce.RateLimit.RequestsPerSecond, cfg.Commerce.RateLimit.Burst)
	}
}

func TestLoadOverrides(t *testing.T) {
	t.Parallel()

	cfg, err := load(mapLookup(map[string]string{
		"APP_ENV":                     "PRODUCTION",
		"SERVICE_NAME":                "accounts",
		"HTTP_ADDR":                   "127.0.0.1:9090",
		"HTTP_SHUTDOWN_TIMEOUT":       "25s",
		"DATABASE_URL":                "postgres://commerce:secret@db:5432/commerce?sslmode=verify-full",
		"DB_MAX_CONNS":                "50",
		"DB_MIN_IDLE_CONNS":           "5",
		"DB_MAX_CONN_LIFETIME":        "45m",
		"DB_MAX_CONN_LIFETIME_JITTER": "2m",
		"DB_MAX_CONN_IDLE_TIME":       "10m",
		"DB_HEALTH_CHECK_PERIOD":      "20s",
		"DB_PING_TIMEOUT":             "3s",
		"LOG_LEVEL":                   "DEBUG",
		"HTTP_READ_HEADER_TIMEOUT":    "4s",
		"HTTP_TLS_CERT_FILE":          "/run/secrets/tls.crt",
		"HTTP_TLS_KEY_FILE":           "/run/secrets/tls.key",
		"PASETO_V4_LOCAL_KEY":         testPasetoKey,
		"ACCESS_TOKEN_TTL":            "20m",
		"REFRESH_TOKEN_TTL":           "240h",
		"AUTH_RATE_LIMIT_RPS":         "3.5",
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
		"DATABASE_URL":        "postgres://commerce:secret@localhost:5432/commerce?sslmode=disable",
		"PASETO_V4_LOCAL_KEY": testPasetoKey,
	}), "order-service", "127.0.0.1:8082")
	if err != nil {
		t.Fatalf("loadWithDefaults() error = %v", err)
	}
	if cfg.ServiceName != "order-service" || cfg.HTTP.Address != "127.0.0.1:8082" {
		t.Fatalf("process defaults = (%q, %q)", cfg.ServiceName, cfg.HTTP.Address)
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
			name: "invalid commerce rate",
			mutate: func(values map[string]string) {
				values["COMMERCE_RATE_LIMIT_RPS"] = "0"
			},
			wantErr: "COMMERCE_RATE_LIMIT_RPS",
		},
		{
			name: "incomplete TLS configuration",
			mutate: func(values map[string]string) {
				values["HTTP_TLS_CERT_FILE"] = "/tmp/cert.pem"
			},
			wantErr: "HTTP_TLS_CERT_FILE",
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
			name: "database operation exceeds write timeout",
			mutate: func(values map[string]string) {
				values["DB_OPERATION_TIMEOUT"] = "15s"
			},
			wantErr: "DB_OPERATION_TIMEOUT",
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
