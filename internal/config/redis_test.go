package config

import (
	"strings"
	"testing"
	"time"
)

const testDatabaseURL = "postgres://commerce:secret@localhost:5432/commerce?sslmode=disable"

func TestLoadOrderProductCacheConfig(t *testing.T) {
	t.Parallel()
	cfg, err := loadWithDefaults(mapLookup(map[string]string{
		"DATABASE_URL":                    testDatabaseURL,
		"CACHE_REDIS_ADDR":                "127.0.0.1:6379",
		"CACHE_REDIS_PASSWORD":            "cache-secret",
		"CACHE_REDIS_DB":                  "3",
		"PRODUCT_CACHE_TTL":               "45s",
		"PRODUCT_CACHE_OPERATION_TIMEOUT": "150ms",
	}), "order-service", "127.0.0.1:8082")
	if err != nil {
		t.Fatalf("loadWithDefaults() error = %v", err)
	}
	if !cfg.ProductCache.Enabled || cfg.ProductCache.Redis.Database != 3 ||
		cfg.ProductCache.Redis.Password != "cache-secret" || cfg.ProductCache.TTL != 45*time.Second ||
		cfg.ProductCache.OperationTimeout != 150*time.Millisecond {
		t.Fatalf("product cache config = %+v", cfg.ProductCache)
	}
	if cfg.Jobs != (JobClientConfig{}) {
		t.Fatalf("order-service loaded unowned jobs config: %+v", cfg.Jobs)
	}
}

func TestLoadUserJobClientConfig(t *testing.T) {
	t.Parallel()
	cfg, err := load(mapLookup(map[string]string{
		"DATABASE_URL":         testDatabaseURL,
		"PASETO_V4_LOCAL_KEY":  testPasetoKey,
		"QUEUE_REDIS_ADDR":     "127.0.0.1:6379",
		"QUEUE_REDIS_PASSWORD": "queue-secret",
		"QUEUE_REDIS_DB":       "4",
		"JOB_QUEUE":            "auth:maintenance",
		"JOB_ENQUEUE_TIMEOUT":  "300ms",
		"JOB_TASK_TIMEOUT":     "12s",
		"JOB_UNIQUE_TTL":       "2m",
	}))
	if err != nil {
		t.Fatalf("load() error = %v", err)
	}
	if !cfg.Jobs.Enabled || cfg.Jobs.Redis.Database != 4 || cfg.Jobs.Queue != "auth:maintenance" ||
		cfg.Jobs.EnqueueTimeout != 300*time.Millisecond || cfg.Jobs.TaskTimeout != 12*time.Second ||
		cfg.Jobs.UniqueTTL != 2*time.Minute {
		t.Fatalf("job client config = %+v", cfg.Jobs)
	}
	if cfg.ProductCache != (ProductCacheConfig{}) {
		t.Fatalf("user-service loaded unowned cache config: %+v", cfg.ProductCache)
	}
}

func TestLoadPaymentIgnoresRedisSettings(t *testing.T) {
	t.Parallel()
	cfg, err := loadWithDefaults(mapLookup(map[string]string{
		"DATABASE_URL":      testDatabaseURL,
		"CACHE_REDIS_ADDR":  "not-an-address",
		"QUEUE_REDIS_ADDR":  "not-an-address",
		"PRODUCT_CACHE_TTL": "invalid",
		"JOB_TASK_TIMEOUT":  "invalid",
	}), "payment-service", "127.0.0.1:8083")
	if err != nil {
		t.Fatalf("payment-service rejected unowned Redis settings: %v", err)
	}
	if cfg.ProductCache != (ProductCacheConfig{}) || cfg.Jobs != (JobClientConfig{}) {
		t.Fatalf("payment-service Redis config = (%+v, %+v)", cfg.ProductCache, cfg.Jobs)
	}
}

func TestLoadRedisConfigSecurityValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		values  map[string]string
		env     string
		wantErr string
	}{
		{name: "missing password", values: map[string]string{"CACHE_REDIS_ADDR": "127.0.0.1:6379"}, env: "local", wantErr: "PASSWORD"},
		{name: "non-loopback plaintext", values: map[string]string{"CACHE_REDIS_ADDR": "10.0.0.2:6379", "CACHE_REDIS_PASSWORD": "secret"}, env: "local", wantErr: "loopback"},
		{name: "production plaintext", values: map[string]string{"CACHE_REDIS_ADDR": "redis.internal:6379", "CACHE_REDIS_PASSWORD": "secret"}, env: "production", wantErr: "TLS is required"},
		{name: "incomplete TLS", values: map[string]string{"CACHE_REDIS_ADDR": "redis.internal:6379", "CACHE_REDIS_PASSWORD": "secret", "CACHE_REDIS_TLS_CA_FILE": "ca.pem"}, env: "production", wantErr: "SERVER_NAME"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, _, err := loadRedisConfig(mapLookup(test.values), "CACHE", 0, test.env, true); err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("loadRedisConfig() error = %v, want %q", err, test.wantErr)
			}
		})
	}
}

func TestLoadJobWorkerDefaults(t *testing.T) {
	t.Parallel()
	cfg, err := loadJobWorker(mapLookup(map[string]string{
		"DATABASE_URL":                 testDatabaseURL,
		"QUEUE_REDIS_ADDR":             "127.0.0.1:6379",
		"QUEUE_REDIS_PASSWORD":         "queue-secret",
		"HTTP_ADDR":                    "invalid",
		"GRPC_ADDR":                    "invalid",
		"AUTH_RATE_LIMIT_RPS":          "invalid",
		"DB_COMMIT_RESOLUTION_TIMEOUT": "invalid",
	}))
	if err != nil {
		t.Fatalf("loadJobWorker() error = %v", err)
	}
	if cfg.ServiceName != "job-worker" || cfg.Queue != "maintenance" || cfg.Concurrency != 4 ||
		cfg.TaskTimeout != 10*time.Second || cfg.ShutdownTimeout != 15*time.Second ||
		cfg.CleanupInterval != time.Minute || cfg.SessionCleanupBatchSize != 100 || cfg.Redis.Database != 1 {
		t.Fatalf("worker config = %+v", cfg)
	}
}

func TestLoadJobWorkerRejectsInvalidConfiguration(t *testing.T) {
	t.Parallel()
	base := map[string]string{
		"DATABASE_URL":         testDatabaseURL,
		"QUEUE_REDIS_ADDR":     "127.0.0.1:6379",
		"QUEUE_REDIS_PASSWORD": "queue-secret",
	}
	tests := []struct {
		name    string
		key     string
		value   string
		wantErr string
	}{
		{name: "PASETO isolation", key: "PASETO_V4_LOCAL_KEY", value: testPasetoKey, wantErr: "must not be configured"},
		{name: "queue", key: "JOB_QUEUE", value: "Invalid Queue", wantErr: "JOB_QUEUE"},
		{name: "database budget", key: "DB_OPERATION_TIMEOUT", value: "10s", wantErr: "shorter than JOB_TASK_TIMEOUT"},
		{name: "shutdown budget", key: "JOB_WORKER_SHUTDOWN_TIMEOUT", value: "5s", wantErr: "shorter than JOB_TASK_TIMEOUT"},
		{name: "fractional task timeout", key: "JOB_TASK_TIMEOUT", value: "1500ms", wantErr: "whole-second"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			values := map[string]string{}
			for key, value := range base {
				values[key] = value
			}
			values[test.key] = test.value
			if _, err := loadJobWorker(mapLookup(values)); err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("loadJobWorker() error = %v, want %q", err, test.wantErr)
			}
		})
	}
}

func TestLoadJobAdminOwnsOnlyQueueConfiguration(t *testing.T) {
	t.Parallel()
	cfg, err := loadJobAdmin(mapLookup(map[string]string{
		"QUEUE_REDIS_ADDR":           "127.0.0.1:6379",
		"QUEUE_REDIS_PASSWORD":       "queue-secret",
		"DATABASE_URL":               "invalid",
		"DB_OPERATION_TIMEOUT":       "invalid",
		"JOB_WORKER_CONCURRENCY":     "invalid",
		"SESSION_CLEANUP_BATCH_SIZE": "invalid",
	}))
	if err != nil {
		t.Fatalf("loadJobAdmin() error = %v", err)
	}
	if cfg.ServiceName != "job-admin" || cfg.Queue != "maintenance" || cfg.Redis.Database != 1 {
		t.Fatalf("job admin config = %+v", cfg)
	}
}

func TestLoadRequestBudgetIncludesOptionalRedisWork(t *testing.T) {
	t.Parallel()
	_, err := loadWithDefaults(mapLookup(map[string]string{
		"DATABASE_URL":                    testDatabaseURL,
		"HTTP_WRITE_TIMEOUT":              "19s",
		"HTTP_SHUTDOWN_TIMEOUT":           "19s",
		"CACHE_REDIS_ADDR":                "127.0.0.1:6379",
		"CACHE_REDIS_PASSWORD":            "secret",
		"PRODUCT_CACHE_OPERATION_TIMEOUT": "1s",
	}), "order-service", "127.0.0.1:8082")
	if err == nil || !strings.Contains(err.Error(), "must fit within HTTP_WRITE_TIMEOUT") {
		t.Fatalf("loadWithDefaults() error = %v, want Redis-inclusive budget error", err)
	}
}
