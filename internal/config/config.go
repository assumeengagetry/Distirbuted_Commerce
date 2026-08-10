package config

import (
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"math"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Config contains all process configuration. Secrets remain in environment
// variables and must not be logged with this struct.
type Config struct {
	Environment string
	ServiceName string
	HTTP        HTTPConfig
	Database    DatabaseConfig
	Log         LogConfig
	Auth        AuthConfig
	Commerce    CommerceConfig
}

type HTTPConfig struct {
	Address           string
	TLSCertFile       string
	TLSKeyFile        string
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	ShutdownTimeout   time.Duration
}

type DatabaseConfig struct {
	URL                   string
	MaxConns              int32
	MinIdleConns          int32
	MaxConnLifetime       time.Duration
	MaxConnLifetimeJitter time.Duration
	MaxConnIdleTime       time.Duration
	HealthCheckPeriod     time.Duration
	PingTimeout           time.Duration
	OperationTimeout      time.Duration
	LockTimeout           time.Duration
}

type LogConfig struct {
	Level string
}

type AuthConfig struct {
	PasetoV4LocalKey     string
	Issuer               string
	AccessTokenTTL       time.Duration
	RefreshTokenTTL      time.Duration
	ClockSkew            time.Duration
	RefreshReuseGrace    time.Duration
	Argon2MemoryKiB      uint32
	Argon2Iterations     uint32
	Argon2Parallelism    uint8
	Argon2MaxConcurrency int
	RateLimit            RateLimitConfig
}

type RateLimitConfig struct {
	RequestsPerSecond float64
	Burst             int
	EntryTTL          time.Duration
	MaxEntries        int
}

type CommerceConfig struct {
	RateLimit RateLimitConfig
}

type lookupEnv func(string) (string, bool)

func Load() (Config, error) {
	return loadWithDefaults(os.LookupEnv, "user-service", "127.0.0.1:8081")
}

func load(lookup lookupEnv) (Config, error) {
	return loadWithDefaults(lookup, "user-service", "127.0.0.1:8081")
}

func LoadForService(defaultServiceName, defaultHTTPAddress string) (Config, error) {
	if strings.TrimSpace(defaultServiceName) == "" {
		return Config{}, fmt.Errorf("default service name must not be empty")
	}
	if err := validateHTTPAddress(defaultHTTPAddress); err != nil {
		return Config{}, fmt.Errorf("invalid default HTTP address: %w", err)
	}
	return loadWithDefaults(os.LookupEnv, defaultServiceName, defaultHTTPAddress)
}

func loadWithDefaults(lookup lookupEnv, defaultServiceName, defaultHTTPAddress string) (Config, error) {
	environment := strings.ToLower(valueOrDefault(lookup, "APP_ENV", "local"))
	switch environment {
	case "local", "test", "production":
	default:
		return Config{}, fmt.Errorf("APP_ENV must be one of local, test, or production")
	}

	serviceName := valueOrDefault(lookup, "SERVICE_NAME", defaultServiceName)
	if serviceName == "" {
		return Config{}, fmt.Errorf("SERVICE_NAME must not be empty")
	}

	databaseURL, ok := lookup("DATABASE_URL")
	databaseURL = strings.TrimSpace(databaseURL)
	if !ok || databaseURL == "" {
		return Config{}, fmt.Errorf("DATABASE_URL is required")
	}
	if environment == "production" {
		if err := validateProductionDatabaseURL(databaseURL); err != nil {
			return Config{}, err
		}
	}

	logLevel := strings.ToLower(valueOrDefault(lookup, "LOG_LEVEL", "info"))
	switch logLevel {
	case "debug", "info", "warn", "error":
	default:
		return Config{}, fmt.Errorf("LOG_LEVEL must be one of debug, info, warn, or error")
	}

	httpAddress := valueOrDefault(lookup, "HTTP_ADDR", defaultHTTPAddress)
	if err := validateHTTPAddress(httpAddress); err != nil {
		return Config{}, err
	}
	tlsCertFile := valueOrDefault(lookup, "HTTP_TLS_CERT_FILE", "")
	tlsKeyFile := valueOrDefault(lookup, "HTTP_TLS_KEY_FILE", "")
	if (tlsCertFile == "") != (tlsKeyFile == "") {
		return Config{}, fmt.Errorf("HTTP_TLS_CERT_FILE and HTTP_TLS_KEY_FILE must be configured together")
	}
	if environment == "production" && tlsCertFile == "" {
		return Config{}, fmt.Errorf("HTTP TLS certificate and key are required in production")
	}

	readHeaderTimeout, err := positiveDuration(lookup, "HTTP_READ_HEADER_TIMEOUT", 5*time.Second)
	if err != nil {
		return Config{}, err
	}
	readTimeout, err := positiveDuration(lookup, "HTTP_READ_TIMEOUT", 10*time.Second)
	if err != nil {
		return Config{}, err
	}
	writeTimeout, err := positiveDuration(lookup, "HTTP_WRITE_TIMEOUT", 15*time.Second)
	if err != nil {
		return Config{}, err
	}
	idleTimeout, err := positiveDuration(lookup, "HTTP_IDLE_TIMEOUT", 60*time.Second)
	if err != nil {
		return Config{}, err
	}
	shutdownTimeout, err := positiveDuration(lookup, "HTTP_SHUTDOWN_TIMEOUT", 20*time.Second)
	if err != nil {
		return Config{}, err
	}
	if shutdownTimeout < writeTimeout {
		return Config{}, fmt.Errorf("HTTP_SHUTDOWN_TIMEOUT must be greater than or equal to HTTP_WRITE_TIMEOUT")
	}

	maxConns, err := integer(lookup, "DB_MAX_CONNS", 20, 1, 200)
	if err != nil {
		return Config{}, err
	}
	minIdleConns, err := integer(lookup, "DB_MIN_IDLE_CONNS", 2, 0, 200)
	if err != nil {
		return Config{}, err
	}
	if minIdleConns > maxConns {
		return Config{}, fmt.Errorf("DB_MIN_IDLE_CONNS must not exceed DB_MAX_CONNS")
	}

	maxConnLifetime, err := positiveDuration(lookup, "DB_MAX_CONN_LIFETIME", time.Hour)
	if err != nil {
		return Config{}, err
	}
	maxConnLifetimeJitter, err := nonNegativeDuration(lookup, "DB_MAX_CONN_LIFETIME_JITTER", 5*time.Minute)
	if err != nil {
		return Config{}, err
	}
	if maxConnLifetimeJitter > maxConnLifetime {
		return Config{}, fmt.Errorf("DB_MAX_CONN_LIFETIME_JITTER must not exceed DB_MAX_CONN_LIFETIME")
	}
	maxConnIdleTime, err := positiveDuration(lookup, "DB_MAX_CONN_IDLE_TIME", 30*time.Minute)
	if err != nil {
		return Config{}, err
	}
	healthCheckPeriod, err := positiveDuration(lookup, "DB_HEALTH_CHECK_PERIOD", time.Minute)
	if err != nil {
		return Config{}, err
	}
	pingTimeout, err := positiveDuration(lookup, "DB_PING_TIMEOUT", 2*time.Second)
	if err != nil {
		return Config{}, err
	}
	if pingTimeout > writeTimeout {
		return Config{}, fmt.Errorf("DB_PING_TIMEOUT must not exceed HTTP_WRITE_TIMEOUT")
	}
	operationTimeout, err := boundedDuration(lookup, "DB_OPERATION_TIMEOUT", 5*time.Second, 500*time.Millisecond, 30*time.Second)
	if err != nil {
		return Config{}, err
	}
	if operationTimeout >= writeTimeout {
		return Config{}, fmt.Errorf("DB_OPERATION_TIMEOUT must be shorter than HTTP_WRITE_TIMEOUT")
	}
	lockTimeout, err := boundedDuration(lookup, "DB_LOCK_TIMEOUT", 2*time.Second, 100*time.Millisecond, 10*time.Second)
	if err != nil {
		return Config{}, err
	}
	if lockTimeout >= operationTimeout {
		return Config{}, fmt.Errorf("DB_LOCK_TIMEOUT must be shorter than DB_OPERATION_TIMEOUT")
	}

	pasetoKey, ok := lookup("PASETO_V4_LOCAL_KEY")
	pasetoKey = strings.TrimSpace(pasetoKey)
	decodedKey, keyErr := hex.DecodeString(pasetoKey)
	if !ok || keyErr != nil || len(decodedKey) != 32 {
		return Config{}, fmt.Errorf("PASETO_V4_LOCAL_KEY must be a 32-byte hex key")
	}
	issuer := valueOrDefault(lookup, "TOKEN_ISSUER", "distributed-commerce/user-service")
	if issuer == "" || len(issuer) > 128 || strings.ContainsAny(issuer, "\r\n\t") {
		return Config{}, fmt.Errorf("TOKEN_ISSUER must be between 1 and 128 characters")
	}
	accessTokenTTL, err := boundedDuration(lookup, "ACCESS_TOKEN_TTL", 15*time.Minute, time.Minute, time.Hour)
	if err != nil {
		return Config{}, err
	}
	refreshTokenTTL, err := boundedDuration(lookup, "REFRESH_TOKEN_TTL", 7*24*time.Hour, time.Hour, 90*24*time.Hour)
	if err != nil {
		return Config{}, err
	}
	if refreshTokenTTL <= accessTokenTTL {
		return Config{}, fmt.Errorf("REFRESH_TOKEN_TTL must exceed ACCESS_TOKEN_TTL")
	}
	clockSkew, err := boundedNonNegativeDuration(lookup, "TOKEN_CLOCK_SKEW", 30*time.Second, 5*time.Minute)
	if err != nil {
		return Config{}, err
	}
	refreshReuseGrace, err := boundedNonNegativeDuration(lookup, "REFRESH_REUSE_GRACE", 5*time.Second, time.Minute)
	if err != nil {
		return Config{}, err
	}
	argon2Memory, err := integer(lookup, "ARGON2_MEMORY_KIB", 64*1024, 19*1024, 256*1024)
	if err != nil {
		return Config{}, err
	}
	argon2Iterations, err := integer(lookup, "ARGON2_ITERATIONS", 3, 1, 10)
	if err != nil {
		return Config{}, err
	}
	argon2Parallelism, err := integer(lookup, "ARGON2_PARALLELISM", 2, 1, 16)
	if err != nil {
		return Config{}, err
	}
	argon2MaxConcurrency, err := integer(lookup, "ARGON2_MAX_CONCURRENCY", 2, 1, 32)
	if err != nil {
		return Config{}, err
	}
	rateLimitRPS, err := decimal(lookup, "AUTH_RATE_LIMIT_RPS", 2, 0.1, 1000)
	if err != nil {
		return Config{}, err
	}
	rateLimitBurst, err := integer(lookup, "AUTH_RATE_LIMIT_BURST", 5, 1, 100)
	if err != nil {
		return Config{}, err
	}
	rateLimitEntryTTL, err := boundedDuration(lookup, "AUTH_RATE_LIMIT_ENTRY_TTL", 10*time.Minute, time.Minute, time.Hour)
	if err != nil {
		return Config{}, err
	}
	rateLimitMaxEntries, err := integer(lookup, "AUTH_RATE_LIMIT_MAX_ENTRIES", 10000, 100, 100000)
	if err != nil {
		return Config{}, err
	}
	commerceRateLimitRPS, err := decimal(lookup, "COMMERCE_RATE_LIMIT_RPS", 20, 0.1, 1000)
	if err != nil {
		return Config{}, err
	}
	commerceRateLimitBurst, err := integer(lookup, "COMMERCE_RATE_LIMIT_BURST", 40, 1, 1000)
	if err != nil {
		return Config{}, err
	}
	commerceRateLimitEntryTTL, err := boundedDuration(
		lookup, "COMMERCE_RATE_LIMIT_ENTRY_TTL", 10*time.Minute, time.Minute, time.Hour,
	)
	if err != nil {
		return Config{}, err
	}
	commerceRateLimitMaxEntries, err := integer(
		lookup, "COMMERCE_RATE_LIMIT_MAX_ENTRIES", 10000, 100, 100000,
	)
	if err != nil {
		return Config{}, err
	}

	return Config{
		Environment: environment,
		ServiceName: serviceName,
		HTTP: HTTPConfig{
			Address:           httpAddress,
			TLSCertFile:       tlsCertFile,
			TLSKeyFile:        tlsKeyFile,
			ReadHeaderTimeout: readHeaderTimeout,
			ReadTimeout:       readTimeout,
			WriteTimeout:      writeTimeout,
			IdleTimeout:       idleTimeout,
			ShutdownTimeout:   shutdownTimeout,
		},
		Database: DatabaseConfig{
			URL:                   databaseURL,
			MaxConns:              maxConns,
			MinIdleConns:          minIdleConns,
			MaxConnLifetime:       maxConnLifetime,
			MaxConnLifetimeJitter: maxConnLifetimeJitter,
			MaxConnIdleTime:       maxConnIdleTime,
			HealthCheckPeriod:     healthCheckPeriod,
			PingTimeout:           pingTimeout,
			OperationTimeout:      operationTimeout,
			LockTimeout:           lockTimeout,
		},
		Log: LogConfig{Level: logLevel},
		Auth: AuthConfig{
			PasetoV4LocalKey:     pasetoKey,
			Issuer:               issuer,
			AccessTokenTTL:       accessTokenTTL,
			RefreshTokenTTL:      refreshTokenTTL,
			ClockSkew:            clockSkew,
			RefreshReuseGrace:    refreshReuseGrace,
			Argon2MemoryKiB:      uint32(argon2Memory),
			Argon2Iterations:     uint32(argon2Iterations),
			Argon2Parallelism:    uint8(argon2Parallelism),
			Argon2MaxConcurrency: int(argon2MaxConcurrency),
			RateLimit: RateLimitConfig{
				RequestsPerSecond: rateLimitRPS,
				Burst:             int(rateLimitBurst),
				EntryTTL:          rateLimitEntryTTL,
				MaxEntries:        int(rateLimitMaxEntries),
			},
		},
		Commerce: CommerceConfig{RateLimit: RateLimitConfig{
			RequestsPerSecond: commerceRateLimitRPS,
			Burst:             int(commerceRateLimitBurst),
			EntryTTL:          commerceRateLimitEntryTTL,
			MaxEntries:        int(commerceRateLimitMaxEntries),
		}},
	}, nil
}

func valueOrDefault(lookup lookupEnv, key, fallback string) string {
	value, ok := lookup(key)
	if !ok {
		return fallback
	}
	return strings.TrimSpace(value)
}

func positiveDuration(lookup lookupEnv, key string, fallback time.Duration) (time.Duration, error) {
	raw, ok := lookup(key)
	if !ok {
		return fallback, nil
	}

	value, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", key)
	}
	return value, nil
}

func nonNegativeDuration(lookup lookupEnv, key string, fallback time.Duration) (time.Duration, error) {
	raw, ok := lookup(key)
	if !ok {
		return fallback, nil
	}

	value, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil || value < 0 {
		return 0, fmt.Errorf("%s must be a non-negative duration", key)
	}
	return value, nil
}

func boundedDuration(lookup lookupEnv, key string, fallback, minimum, maximum time.Duration) (time.Duration, error) {
	value, err := positiveDuration(lookup, key, fallback)
	if err != nil {
		return 0, err
	}
	if value < minimum || value > maximum {
		return 0, fmt.Errorf("%s must be between %s and %s", key, minimum, maximum)
	}
	return value, nil
}

func boundedNonNegativeDuration(lookup lookupEnv, key string, fallback, maximum time.Duration) (time.Duration, error) {
	value, err := nonNegativeDuration(lookup, key, fallback)
	if err != nil {
		return 0, err
	}
	if value > maximum {
		return 0, fmt.Errorf("%s must not exceed %s", key, maximum)
	}
	return value, nil
}

func integer(lookup lookupEnv, key string, fallback, minimum, maximum int32) (int32, error) {
	raw, ok := lookup(key)
	if !ok {
		return fallback, nil
	}

	value, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 32)
	if err != nil || value < int64(minimum) || value > int64(maximum) {
		return 0, fmt.Errorf("%s must be an integer between %d and %d", key, minimum, maximum)
	}
	return int32(value), nil
}

func decimal(lookup lookupEnv, key string, fallback, minimum, maximum float64) (float64, error) {
	raw, ok := lookup(key)
	if !ok {
		return fallback, nil
	}
	value, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < minimum || value > maximum {
		return 0, fmt.Errorf("%s must be a number between %g and %g", key, minimum, maximum)
	}
	return value, nil
}

func validateHTTPAddress(address string) error {
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("HTTP_ADDR must be a host:port listen address")
	}

	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return fmt.Errorf("HTTP_ADDR must contain a numeric port between 1 and 65535")
	}
	return nil
}

func validateProductionDatabaseURL(databaseURL string) error {
	parsed, err := url.Parse(databaseURL)
	if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") || parsed.Host == "" {
		return fmt.Errorf("DATABASE_URL must be a PostgreSQL URL in production")
	}
	if parsed.Query().Get("sslmode") != "verify-full" {
		return fmt.Errorf("DATABASE_URL must use sslmode=verify-full in production")
	}
	pgxConfig, err := pgx.ParseConfig(databaseURL)
	if err != nil || !verifiedDatabaseTarget(pgxConfig.Host, pgxConfig.TLSConfig) {
		return fmt.Errorf("DATABASE_URL must resolve to verified TLS targets in production")
	}
	for _, fallback := range pgxConfig.Fallbacks {
		if !verifiedDatabaseTarget(fallback.Host, fallback.TLSConfig) {
			return fmt.Errorf("DATABASE_URL must resolve to verified TLS targets in production")
		}
	}
	return nil
}

func verifiedDatabaseTarget(host string, tlsConfig *tls.Config) bool {
	return host != "" && !strings.HasPrefix(host, "/") && tlsConfig != nil && !tlsConfig.InsecureSkipVerify
}
