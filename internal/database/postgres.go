package database

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/semconv/v1.40.0"

	"github.com/assumeengagetry/distributed-commerce/internal/config"
	"github.com/assumeengagetry/distributed-commerce/internal/observability"
)

var errInvalidDatabaseURL = errors.New("DATABASE_URL is not a valid PostgreSQL connection string")

func Open(
	ctx context.Context,
	cfg config.DatabaseConfig,
	telemetry observability.Providers,
) (*pgxpool.Pool, error) {
	poolConfig, err := newPoolConfig(cfg)
	if err != nil {
		return nil, err
	}
	if telemetry.Enabled() {
		poolConfig.ConnConfig.Tracer = otelpgx.NewTracer(
			otelpgx.WithTracerProvider(telemetry.TracerProvider),
			otelpgx.WithMeterProvider(telemetry.MeterProvider),
			otelpgx.WithDisableSQLStatementInAttributes(),
			otelpgx.WithDisableConnectionDetailsInAttributes(),
			otelpgx.WithDisableQuerySpanNamePrefix(),
			otelpgx.WithSpanNameFunc(telemetryQueryName),
		)
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return nil, fmt.Errorf("create PostgreSQL pool: %w", err)
	}
	if telemetry.Enabled() {
		if err := otelpgx.RecordStats(
			pool,
			otelpgx.WithStatsMeterProvider(telemetry.MeterProvider),
			otelpgx.WithStatsAttributes(semconv.DBClientConnectionPoolName("postgres")),
		); err != nil {
			pool.Close()
			return nil, fmt.Errorf("record PostgreSQL pool metrics: %w", err)
		}
	}

	pingCtx, cancel := context.WithTimeout(ctx, cfg.PingTimeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping PostgreSQL: %w", err)
	}

	return pool, nil
}

func telemetryQueryName(statement string) string {
	trimmed := strings.TrimSpace(statement)
	if strings.HasPrefix(trimmed, "-- name:") {
		fields := strings.Fields(trimmed)
		if len(fields) >= 3 {
			return fields[2]
		}
	}
	fields := strings.Fields(trimmed)
	if len(fields) == 0 {
		return "SQL"
	}
	operation := strings.ToUpper(fields[0])
	switch operation {
	case "SELECT", "INSERT", "UPDATE", "DELETE", "BEGIN", "COMMIT", "ROLLBACK":
		return operation
	default:
		return "SQL"
	}
}

func newPoolConfig(cfg config.DatabaseConfig) (*pgxpool.Config, error) {
	poolConfig, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		return nil, errInvalidDatabaseURL
	}

	poolConfig.MaxConns = cfg.MaxConns
	poolConfig.MinConns = 0
	poolConfig.MinIdleConns = cfg.MinIdleConns
	poolConfig.MaxConnLifetime = cfg.MaxConnLifetime
	poolConfig.MaxConnLifetimeJitter = cfg.MaxConnLifetimeJitter
	poolConfig.MaxConnIdleTime = cfg.MaxConnIdleTime
	poolConfig.HealthCheckPeriod = cfg.HealthCheckPeriod
	poolConfig.ConnConfig.ConnectTimeout = cfg.PingTimeout
	if cfg.OperationTimeout > 0 {
		poolConfig.ConnConfig.RuntimeParams["statement_timeout"] = postgresDuration(cfg.OperationTimeout)
	}

	return poolConfig, nil
}
