package database

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/assumeengagetry/distributed-commerce/internal/config"
)

var errInvalidDatabaseURL = errors.New("DATABASE_URL is not a valid PostgreSQL connection string")

func Open(ctx context.Context, cfg config.DatabaseConfig) (*pgxpool.Pool, error) {
	poolConfig, err := newPoolConfig(cfg)
	if err != nil {
		return nil, err
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return nil, fmt.Errorf("create PostgreSQL pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, cfg.PingTimeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping PostgreSQL: %w", err)
	}

	return pool, nil
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
