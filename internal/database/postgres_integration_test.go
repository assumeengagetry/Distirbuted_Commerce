//go:build integration

package database

import (
	"context"
	"os"
	"testing"
	"time"

	store "github.com/assumeengagetry/distributed-commerce/internal/database/sqlc"
	"github.com/assumeengagetry/distributed-commerce/internal/observability"
)

func TestPostgresConnectionAndMigration(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool, err := Open(ctx, databaseConfig(databaseURL), observability.NoopProviders())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer pool.Close()

	value, err := store.New(pool).HealthCheck(ctx)
	if err != nil {
		t.Fatalf("HealthCheck() error = %v", err)
	}
	if value != 1 {
		t.Fatalf("HealthCheck() = %d, want 1", value)
	}
	orderValue, err := store.New(pool).OrderHealthCheck(ctx)
	if err != nil {
		t.Fatalf("OrderHealthCheck() error = %v", err)
	}
	if orderValue != 1 {
		t.Fatalf("OrderHealthCheck() = %d, want 1", orderValue)
	}
	paymentValue, err := store.New(pool).PaymentHealthCheck(ctx)
	if err != nil {
		t.Fatalf("PaymentHealthCheck() error = %v", err)
	}
	if paymentValue != 1 {
		t.Fatalf("PaymentHealthCheck() = %d, want 1", paymentValue)
	}

	var extensionExists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'pgcrypto')`).Scan(&extensionExists); err != nil {
		t.Fatalf("query pgcrypto extension: %v", err)
	}
	if !extensionExists {
		t.Fatal("pgcrypto extension is not installed; run migrations before integration tests")
	}
}
