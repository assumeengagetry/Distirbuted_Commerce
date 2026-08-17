package database

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/assumeengagetry/distributed-commerce/internal/config"
)

func TestNewPoolConfig(t *testing.T) {
	t.Parallel()

	cfg := databaseConfig("postgres://commerce:secret@localhost:5432/commerce?sslmode=disable")
	poolConfig, err := newPoolConfig(cfg)
	if err != nil {
		t.Fatalf("newPoolConfig() error = %v", err)
	}

	if poolConfig.MaxConns != cfg.MaxConns || poolConfig.MinIdleConns != cfg.MinIdleConns {
		t.Errorf("pool size = (%d, %d), want (%d, %d)", poolConfig.MaxConns, poolConfig.MinIdleConns, cfg.MaxConns, cfg.MinIdleConns)
	}
	if poolConfig.MinConns != 0 {
		t.Errorf("MinConns = %d, want 0", poolConfig.MinConns)
	}
	if poolConfig.MaxConnLifetime != cfg.MaxConnLifetime {
		t.Errorf("MaxConnLifetime = %s, want %s", poolConfig.MaxConnLifetime, cfg.MaxConnLifetime)
	}
	if poolConfig.MaxConnLifetimeJitter != cfg.MaxConnLifetimeJitter {
		t.Errorf("MaxConnLifetimeJitter = %s, want %s", poolConfig.MaxConnLifetimeJitter, cfg.MaxConnLifetimeJitter)
	}
	if poolConfig.MaxConnIdleTime != cfg.MaxConnIdleTime {
		t.Errorf("MaxConnIdleTime = %s, want %s", poolConfig.MaxConnIdleTime, cfg.MaxConnIdleTime)
	}
	if poolConfig.HealthCheckPeriod != cfg.HealthCheckPeriod {
		t.Errorf("HealthCheckPeriod = %s, want %s", poolConfig.HealthCheckPeriod, cfg.HealthCheckPeriod)
	}
	if poolConfig.ConnConfig.ConnectTimeout != cfg.PingTimeout {
		t.Errorf("ConnectTimeout = %s, want %s", poolConfig.ConnConfig.ConnectTimeout, cfg.PingTimeout)
	}
	if poolConfig.ConnConfig.RuntimeParams["statement_timeout"] != "5000ms" {
		t.Errorf("statement_timeout = %q, want 5000ms", poolConfig.ConnConfig.RuntimeParams["statement_timeout"])
	}
}

func TestNewPoolConfigOverridesPoolSettingsFromURL(t *testing.T) {
	t.Parallel()

	cfg := databaseConfig("postgres://commerce:secret@localhost:5432/commerce?pool_min_conns=19&pool_min_idle_conns=18&pool_max_conn_lifetime_jitter=30m")
	poolConfig, err := newPoolConfig(cfg)
	if err != nil {
		t.Fatalf("newPoolConfig() error = %v", err)
	}

	if poolConfig.MinConns != 0 || poolConfig.MinIdleConns != cfg.MinIdleConns {
		t.Errorf("URL overrode minimum pool settings: MinConns=%d MinIdleConns=%d", poolConfig.MinConns, poolConfig.MinIdleConns)
	}
	if poolConfig.MaxConnLifetimeJitter != cfg.MaxConnLifetimeJitter {
		t.Errorf("URL overrode lifetime jitter: %s", poolConfig.MaxConnLifetimeJitter)
	}
}

func TestNewPoolConfigDoesNotExposeInvalidURL(t *testing.T) {
	t.Parallel()

	const secret = "do-not-log-this-password"
	cfg := databaseConfig("postgres://commerce:" + secret + "@%gh&%ij")

	_, err := newPoolConfig(cfg)
	if !errors.Is(err, errInvalidDatabaseURL) {
		t.Fatalf("newPoolConfig() error = %v, want %v", err, errInvalidDatabaseURL)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("newPoolConfig() error exposes database password: %v", err)
	}
}

func TestTelemetryQueryNameIsBoundedAndDoesNotExposeSQL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		statement string
		want      string
	}{
		{statement: "-- name: CreateOrder :one\nINSERT INTO orders VALUES ($1)", want: "CreateOrder"},
		{statement: " SELECT secret FROM users WHERE email = $1", want: "SELECT"},
		{statement: "UPDATE accounts SET balance = 0", want: "UPDATE"},
		{statement: "WITH private_data AS (SELECT 1) SELECT * FROM private_data", want: "SQL"},
		{statement: "", want: "SQL"},
	}
	for _, test := range tests {
		if got := telemetryQueryName(test.statement); got != test.want {
			t.Errorf("telemetryQueryName(%q) = %q, want %q", test.statement, got, test.want)
		}
	}
}

func databaseConfig(url string) config.DatabaseConfig {
	return config.DatabaseConfig{
		URL:                     url,
		MaxConns:                20,
		MinIdleConns:            2,
		MaxConnLifetime:         time.Hour,
		MaxConnLifetimeJitter:   5 * time.Minute,
		MaxConnIdleTime:         30 * time.Minute,
		HealthCheckPeriod:       time.Minute,
		PingTimeout:             2 * time.Second,
		OperationTimeout:        5 * time.Second,
		LockTimeout:             2 * time.Second,
		CommitResolutionTimeout: 2 * time.Second,
	}
}
