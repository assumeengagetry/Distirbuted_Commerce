//go:build integration

package productcache

import (
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/assumeengagetry/distributed-commerce/internal/database"
	"github.com/assumeengagetry/distributed-commerce/internal/order"
)

func TestPostgreSQLAndRealRedisCacheInvalidationAndFallback(t *testing.T) {
	ctx := t.Context()
	pool, err := pgxpool.New(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatalf("open test PostgreSQL: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping test PostgreSQL: %v", err)
	}
	redisClient := integrationCacheRedisClient(t)
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	base := database.NewOrderRepository(pool, 2*time.Second, 5*time.Second, 2*time.Second)
	repository, err := NewRepository(base, redisClient, logger, time.Minute, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("NewRepository() error = %v", err)
	}

	adminID := uuid.New()
	accountID := uuid.New()
	productID := uuid.New()
	email := "cache-" + adminID.String() + "@example.com"
	if _, err := pool.Exec(ctx, `
		INSERT INTO users (id, email, password_hash, display_name, role, status)
		VALUES ($1, $2, '$argon2id$integration', 'Cache Admin', 'admin', 'active')
	`, adminID, email); err != nil {
		t.Fatalf("seed cache integration admin: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM inventories WHERE product_id = $1`, productID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM products WHERE id = $1`, productID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM users WHERE id = $1`, adminID)
	})
	if _, err := pool.Exec(ctx, `INSERT INTO accounts (id, user_id, currency) VALUES ($1, $2, 'USD')`, accountID, adminID); err != nil {
		t.Fatalf("seed cache integration account: %v", err)
	}

	created, err := repository.CreateProduct(ctx, adminID, order.CreateProductParams{
		ProductID: productID,
		Request: order.CreateProductRequest{
			SKU: "CACHE.REAL", Name: "Real Redis Product", Description: "integration",
			PriceAmount: 1250, Currency: order.CurrencyUSD, Status: order.ProductStatusActive,
			InitialQuantity: 2,
		},
	})
	if err != nil {
		t.Fatalf("CreateProduct() error = %v", err)
	}
	first, err := repository.GetProduct(ctx, productID)
	if err != nil || !first.Available {
		t.Fatalf("GetProduct(first) = (%+v, %v), want available", first, err)
	}
	if redisClient.Exists(ctx, valueKey(productID)).Val() != 1 {
		t.Fatal("real Redis product value was not populated")
	}
	if _, err := repository.AdjustInventory(ctx, adminID, productID, order.AdjustInventoryRequest{
		ExpectedVersion: created.Inventory.Version, Delta: -2,
	}); err != nil {
		t.Fatalf("AdjustInventory() error = %v", err)
	}
	if redisClient.Exists(ctx, valueKey(productID)).Val() != 0 {
		t.Fatal("real Redis product value survived committed inventory adjustment")
	}
	second, err := repository.GetProduct(ctx, productID)
	if err != nil || second.Available {
		t.Fatalf("GetProduct(after adjustment) = (%+v, %v), want unavailable", second, err)
	}

	outageClient := redis.NewClient(&redis.Options{
		Addr: "127.0.0.1:1", DialTimeout: 20 * time.Millisecond, ReadTimeout: 20 * time.Millisecond,
		WriteTimeout: 20 * time.Millisecond, PoolTimeout: 20 * time.Millisecond, MaxRetries: -1,
		ContextTimeoutEnabled: true,
	})
	t.Cleanup(func() { _ = outageClient.Close() })
	outageRepository, err := NewRepository(base, outageClient, logger, time.Minute, 30*time.Millisecond)
	if err != nil {
		t.Fatalf("NewRepository(outage) error = %v", err)
	}
	fallback, err := outageRepository.GetProduct(ctx, productID)
	if err != nil || fallback.Available {
		t.Fatalf("GetProduct(Redis outage) = (%+v, %v), want PostgreSQL fallback", fallback, err)
	}
}

func integrationCacheRedisClient(t *testing.T) *redis.Client {
	t.Helper()
	if strings.EqualFold(strings.TrimSpace(os.Getenv("APP_ENV")), "production") {
		t.Fatal("Redis integration tests must not run in production")
	}
	address := os.Getenv("TEST_REDIS_ADDR")
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatalf("TEST_REDIS_ADDR must be host:port: %v", err)
	}
	ip := net.ParseIP(host)
	if !strings.EqualFold(host, "localhost") && (ip == nil || !ip.IsLoopback()) {
		t.Fatal("TEST_REDIS_ADDR must be loopback")
	}
	databaseNumber, err := strconv.Atoi(os.Getenv("TEST_REDIS_DB"))
	if err != nil || databaseNumber < 2 || databaseNumber > 15 {
		t.Fatal("TEST_REDIS_DB must be an integer between 2 and 15")
	}
	for name, fallback := range map[string]int{"CACHE_REDIS_DB": 0, "QUEUE_REDIS_DB": 1} {
		configured := fallback
		if raw := os.Getenv(name); raw != "" {
			configured, err = strconv.Atoi(raw)
			if err != nil || configured < 0 || configured > 15 {
				t.Fatalf("%s must be an integer between 0 and 15", name)
			}
		}
		if configured == databaseNumber {
			t.Fatalf("TEST_REDIS_DB must differ from %s", name)
		}
	}
	password := os.Getenv("TEST_REDIS_PASSWORD")
	if password == "" {
		t.Fatal("TEST_REDIS_PASSWORD is required")
	}
	client := redis.NewClient(&redis.Options{
		Addr: address, Username: os.Getenv("TEST_REDIS_USERNAME"), Password: password, DB: databaseNumber,
		DialTimeout: time.Second, ReadTimeout: time.Second, WriteTimeout: time.Second,
		PoolTimeout: time.Second, MaxRetries: -1, ContextTimeoutEnabled: true,
	})
	if err := client.Ping(t.Context()).Err(); err != nil {
		_ = client.Close()
		t.Fatalf("ping test Redis: %v", err)
	}
	if err := client.FlushDB(t.Context()).Err(); err != nil {
		_ = client.Close()
		t.Fatalf("flush test Redis: %v", err)
	}
	t.Cleanup(func() {
		_ = client.FlushDB(context.Background()).Err()
		_ = client.Close()
	})
	return client
}
