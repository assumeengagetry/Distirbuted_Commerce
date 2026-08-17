package config

import (
	"fmt"
	"os"
	"strings"
	"time"
)

type RedisConfig struct {
	Address       string
	Username      string
	Password      string
	Database      int
	DialTimeout   time.Duration
	ReadTimeout   time.Duration
	WriteTimeout  time.Duration
	PoolTimeout   time.Duration
	PoolSize      int
	TLSCAFile     string
	TLSCertFile   string
	TLSKeyFile    string
	TLSServerName string
}

type ProductCacheConfig struct {
	Enabled          bool
	Redis            RedisConfig
	TTL              time.Duration
	OperationTimeout time.Duration
}

type JobClientConfig struct {
	Enabled        bool
	Redis          RedisConfig
	Queue          string
	EnqueueTimeout time.Duration
	TaskTimeout    time.Duration
	UniqueTTL      time.Duration
}

type JobWorkerConfig struct {
	Environment             string
	ServiceName             string
	Database                DatabaseConfig
	Log                     LogConfig
	Redis                   RedisConfig
	Queue                   string
	TaskTimeout             time.Duration
	CleanupInterval         time.Duration
	Concurrency             int
	ShutdownTimeout         time.Duration
	SessionCleanupBatchSize int32
}

type JobAdminConfig struct {
	Environment string
	ServiceName string
	Redis       RedisConfig
	Queue       string
}

func LoadJobWorker() (JobWorkerConfig, error) {
	return loadJobWorker(os.LookupEnv)
}

func LoadJobAdmin() (JobAdminConfig, error) {
	return loadJobAdmin(os.LookupEnv)
}

func loadProductCacheConfig(lookup lookupEnv, environment string) (ProductCacheConfig, error) {
	redisConfig, enabled, err := loadRedisConfig(lookup, "CACHE", 0, environment, false)
	if err != nil || !enabled {
		return ProductCacheConfig{}, err
	}
	ttl, err := boundedDuration(lookup, "PRODUCT_CACHE_TTL", 30*time.Second, time.Second, time.Hour)
	if err != nil {
		return ProductCacheConfig{}, err
	}
	operationTimeout, err := boundedDuration(
		lookup, "PRODUCT_CACHE_OPERATION_TIMEOUT", 100*time.Millisecond, 10*time.Millisecond, time.Second,
	)
	if err != nil {
		return ProductCacheConfig{}, err
	}
	return ProductCacheConfig{Enabled: true, Redis: redisConfig, TTL: ttl, OperationTimeout: operationTimeout}, nil
}

func loadJobClientConfig(lookup lookupEnv, environment string) (JobClientConfig, error) {
	redisConfig, enabled, err := loadRedisConfig(lookup, "QUEUE", 1, environment, false)
	if err != nil || !enabled {
		return JobClientConfig{}, err
	}
	queue, taskTimeout, err := loadJobTaskConfig(lookup)
	if err != nil {
		return JobClientConfig{}, err
	}
	enqueueTimeout, err := boundedDuration(
		lookup, "JOB_ENQUEUE_TIMEOUT", 250*time.Millisecond, 10*time.Millisecond, 2*time.Second,
	)
	if err != nil {
		return JobClientConfig{}, err
	}
	uniqueTTL, err := boundedDuration(lookup, "JOB_UNIQUE_TTL", time.Minute, time.Second, time.Hour)
	if err != nil {
		return JobClientConfig{}, err
	}
	return JobClientConfig{
		Enabled: true, Redis: redisConfig, Queue: queue,
		EnqueueTimeout: enqueueTimeout, TaskTimeout: taskTimeout, UniqueTTL: uniqueTTL,
	}, nil
}

func loadJobWorker(lookup lookupEnv) (JobWorkerConfig, error) {
	environment := strings.ToLower(valueOrDefault(lookup, "APP_ENV", "local"))
	switch environment {
	case "local", "test", "production":
	default:
		return JobWorkerConfig{}, fmt.Errorf("APP_ENV must be one of local, test, or production")
	}
	serviceName := valueOrDefault(lookup, "SERVICE_NAME", "job-worker")
	if serviceName == "" {
		return JobWorkerConfig{}, fmt.Errorf("SERVICE_NAME must not be empty")
	}
	databaseURL := valueOrDefault(lookup, "DATABASE_URL", "")
	if databaseURL == "" {
		return JobWorkerConfig{}, fmt.Errorf("DATABASE_URL is required")
	}
	if environment == "production" {
		if err := validateProductionDatabaseURL(databaseURL); err != nil {
			return JobWorkerConfig{}, err
		}
	}
	databaseConfig, err := loadDatabaseConfig(lookup, databaseURL, 0, false)
	if err != nil {
		return JobWorkerConfig{}, err
	}
	logLevel := strings.ToLower(valueOrDefault(lookup, "LOG_LEVEL", "info"))
	switch logLevel {
	case "debug", "info", "warn", "error":
	default:
		return JobWorkerConfig{}, fmt.Errorf("LOG_LEVEL must be one of debug, info, warn, or error")
	}
	if key, configured := lookup("PASETO_V4_LOCAL_KEY"); configured && strings.TrimSpace(key) != "" {
		return JobWorkerConfig{}, fmt.Errorf("PASETO_V4_LOCAL_KEY must not be configured outside user-service")
	}
	redisConfig, _, err := loadRedisConfig(lookup, "QUEUE", 1, environment, true)
	if err != nil {
		return JobWorkerConfig{}, err
	}
	queue, taskTimeout, err := loadJobTaskConfig(lookup)
	if err != nil {
		return JobWorkerConfig{}, err
	}
	if databaseConfig.OperationTimeout >= taskTimeout {
		return JobWorkerConfig{}, fmt.Errorf("DB_OPERATION_TIMEOUT must be shorter than JOB_TASK_TIMEOUT")
	}
	cleanupInterval, err := boundedDuration(lookup, "JOB_CLEANUP_INTERVAL", time.Minute, time.Minute, 24*time.Hour)
	if err != nil {
		return JobWorkerConfig{}, err
	}
	concurrency, err := integer(lookup, "JOB_WORKER_CONCURRENCY", 4, 1, 64)
	if err != nil {
		return JobWorkerConfig{}, err
	}
	shutdownTimeout, err := boundedDuration(
		lookup, "JOB_WORKER_SHUTDOWN_TIMEOUT", 15*time.Second, time.Second, time.Minute,
	)
	if err != nil {
		return JobWorkerConfig{}, err
	}
	if shutdownTimeout < taskTimeout {
		return JobWorkerConfig{}, fmt.Errorf("JOB_WORKER_SHUTDOWN_TIMEOUT must not be shorter than JOB_TASK_TIMEOUT")
	}
	batchSize, err := integer(lookup, "SESSION_CLEANUP_BATCH_SIZE", 100, 1, 1000)
	if err != nil {
		return JobWorkerConfig{}, err
	}
	return JobWorkerConfig{
		Environment: environment, ServiceName: serviceName, Database: databaseConfig,
		Log: LogConfig{Level: logLevel}, Redis: redisConfig, Queue: queue,
		TaskTimeout: taskTimeout, CleanupInterval: cleanupInterval,
		Concurrency: int(concurrency), ShutdownTimeout: shutdownTimeout,
		SessionCleanupBatchSize: batchSize,
	}, nil
}

func loadJobAdmin(lookup lookupEnv) (JobAdminConfig, error) {
	environment := strings.ToLower(valueOrDefault(lookup, "APP_ENV", "local"))
	switch environment {
	case "local", "test", "production":
	default:
		return JobAdminConfig{}, fmt.Errorf("APP_ENV must be one of local, test, or production")
	}
	serviceName := valueOrDefault(lookup, "SERVICE_NAME", "job-admin")
	if serviceName == "" {
		return JobAdminConfig{}, fmt.Errorf("SERVICE_NAME must not be empty")
	}
	if key, configured := lookup("PASETO_V4_LOCAL_KEY"); configured && strings.TrimSpace(key) != "" {
		return JobAdminConfig{}, fmt.Errorf("PASETO_V4_LOCAL_KEY must not be configured outside user-service")
	}
	redisConfig, _, err := loadRedisConfig(lookup, "QUEUE", 1, environment, true)
	if err != nil {
		return JobAdminConfig{}, err
	}
	queue := valueOrDefault(lookup, "JOB_QUEUE", "maintenance")
	if !validQueueName(queue) {
		return JobAdminConfig{}, fmt.Errorf("JOB_QUEUE must contain 1 to 64 lowercase letters, digits, colons, dashes, or underscores")
	}
	return JobAdminConfig{
		Environment: environment, ServiceName: serviceName, Redis: redisConfig, Queue: queue,
	}, nil
}

func loadJobTaskConfig(lookup lookupEnv) (string, time.Duration, error) {
	queue := valueOrDefault(lookup, "JOB_QUEUE", "maintenance")
	if !validQueueName(queue) {
		return "", 0, fmt.Errorf("JOB_QUEUE must contain 1 to 64 lowercase letters, digits, colons, dashes, or underscores")
	}
	taskTimeout, err := boundedDuration(lookup, "JOB_TASK_TIMEOUT", 10*time.Second, time.Second, 30*time.Second)
	if err != nil {
		return "", 0, err
	}
	if taskTimeout%time.Second != 0 {
		return "", 0, fmt.Errorf("JOB_TASK_TIMEOUT must use whole-second precision")
	}
	return queue, taskTimeout, nil
}

func loadRedisConfig(
	lookup lookupEnv,
	prefix string,
	defaultDatabase int32,
	environment string,
	required bool,
) (RedisConfig, bool, error) {
	addressKey := prefix + "_REDIS_ADDR"
	address := valueOrDefault(lookup, addressKey, "")
	if address == "" {
		if required {
			return RedisConfig{}, false, fmt.Errorf("%s is required", addressKey)
		}
		return RedisConfig{}, false, nil
	}
	if err := validateNetworkAddress(addressKey, address); err != nil {
		return RedisConfig{}, false, err
	}
	username := valueOrDefault(lookup, prefix+"_REDIS_USERNAME", "commerce")
	if username == "" || len(username) > 128 || strings.ContainsAny(username, "\x00\r\n\t") {
		return RedisConfig{}, false, fmt.Errorf("%s_REDIS_USERNAME is invalid", prefix)
	}
	password, ok := lookup(prefix + "_REDIS_PASSWORD")
	if !ok || password == "" || len(password) > 512 || strings.ContainsAny(password, "\x00\r\n") {
		return RedisConfig{}, false, fmt.Errorf("%s_REDIS_PASSWORD is required and must not contain control characters", prefix)
	}
	database, err := integer(lookup, prefix+"_REDIS_DB", defaultDatabase, 0, 15)
	if err != nil {
		return RedisConfig{}, false, err
	}
	dialTimeout, err := boundedDuration(lookup, prefix+"_REDIS_DIAL_TIMEOUT", 500*time.Millisecond, 50*time.Millisecond, 5*time.Second)
	if err != nil {
		return RedisConfig{}, false, err
	}
	readTimeout, err := boundedDuration(lookup, prefix+"_REDIS_READ_TIMEOUT", 250*time.Millisecond, 10*time.Millisecond, 5*time.Second)
	if err != nil {
		return RedisConfig{}, false, err
	}
	writeTimeout, err := boundedDuration(lookup, prefix+"_REDIS_WRITE_TIMEOUT", 250*time.Millisecond, 10*time.Millisecond, 5*time.Second)
	if err != nil {
		return RedisConfig{}, false, err
	}
	poolTimeout, err := boundedDuration(lookup, prefix+"_REDIS_POOL_TIMEOUT", time.Second, 50*time.Millisecond, 10*time.Second)
	if err != nil {
		return RedisConfig{}, false, err
	}
	poolSize, err := integer(lookup, prefix+"_REDIS_POOL_SIZE", 10, 1, 100)
	if err != nil {
		return RedisConfig{}, false, err
	}
	tlsCAFile := valueOrDefault(lookup, prefix+"_REDIS_TLS_CA_FILE", "")
	tlsCertFile := valueOrDefault(lookup, prefix+"_REDIS_TLS_CERT_FILE", "")
	tlsKeyFile := valueOrDefault(lookup, prefix+"_REDIS_TLS_KEY_FILE", "")
	tlsServerName := valueOrDefault(lookup, prefix+"_REDIS_TLS_SERVER_NAME", "")
	tlsConfigured := tlsCAFile != "" || tlsCertFile != "" || tlsKeyFile != "" || tlsServerName != ""
	if (tlsCertFile == "") != (tlsKeyFile == "") {
		return RedisConfig{}, false, fmt.Errorf("%s_REDIS_TLS_CERT_FILE and %s_REDIS_TLS_KEY_FILE must be configured together", prefix, prefix)
	}
	if tlsConfigured && (tlsCAFile == "" || tlsServerName == "") {
		return RedisConfig{}, false, fmt.Errorf("%s_REDIS_TLS_CA_FILE and %s_REDIS_TLS_SERVER_NAME are required for Redis TLS", prefix, prefix)
	}
	if environment == "production" && !tlsConfigured {
		return RedisConfig{}, false, fmt.Errorf("%s Redis TLS is required in production", prefix)
	}
	if !tlsConfigured && !isLoopbackAddress(address) {
		return RedisConfig{}, false, fmt.Errorf("%s must be loopback when Redis TLS is disabled", addressKey)
	}
	return RedisConfig{
		Address: address, Username: username, Password: password, Database: int(database),
		DialTimeout: dialTimeout, ReadTimeout: readTimeout, WriteTimeout: writeTimeout,
		PoolTimeout: poolTimeout, PoolSize: int(poolSize), TLSCAFile: tlsCAFile,
		TLSCertFile: tlsCertFile, TLSKeyFile: tlsKeyFile, TLSServerName: tlsServerName,
	}, true, nil
}

func validQueueName(value string) bool {
	if len(value) == 0 || len(value) > 64 {
		return false
	}
	for index := range len(value) {
		character := value[index]
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' ||
			character == ':' || character == '-' || character == '_' {
			continue
		}
		return false
	}
	return true
}
