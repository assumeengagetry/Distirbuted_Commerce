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
	Environment  string
	ServiceName  string
	HTTP         HTTPConfig
	GRPC         GRPCConfig
	Database     DatabaseConfig
	Log          LogConfig
	Auth         AuthConfig
	Commerce     CommerceConfig
	Payment      CommerceConfig
	ProductCache ProductCacheConfig
	Jobs         JobClientConfig
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

type GRPCConfig struct {
	Address              string
	IdentityTarget       string
	CallTimeout          time.Duration
	RequestTimeout       time.Duration
	ShutdownTimeout      time.Duration
	TLSCertFile          string
	TLSKeyFile           string
	TLSCAFile            string
	TLSServerName        string
	TLSAllowedClientURIs []string
}

type DatabaseConfig struct {
	URL                     string
	MaxConns                int32
	MinIdleConns            int32
	MaxConnLifetime         time.Duration
	MaxConnLifetimeJitter   time.Duration
	MaxConnIdleTime         time.Duration
	HealthCheckPeriod       time.Duration
	PingTimeout             time.Duration
	OperationTimeout        time.Duration
	LockTimeout             time.Duration
	CommitResolutionTimeout time.Duration
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
	isIdentityService := defaultServiceName == "user-service"
	isOrderService := defaultServiceName == "order-service"
	isPaymentService := defaultServiceName == "payment-service"
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
	if tlsCertFile == "" && !isLoopbackAddress(httpAddress) {
		return Config{}, fmt.Errorf("HTTP_ADDR must be loopback when HTTP TLS is disabled")
	}
	var grpcAddress string
	var identityTarget string
	var grpcCallTimeout time.Duration
	var grpcRequestTimeout time.Duration
	var grpcShutdownTimeout time.Duration
	var err error
	if isIdentityService {
		grpcAddress = valueOrDefault(lookup, "GRPC_ADDR", "127.0.0.1:9091")
		if err := validateNetworkAddress("GRPC_ADDR", grpcAddress); err != nil {
			return Config{}, err
		}
		grpcRequestTimeout, err = boundedDuration(
			lookup, "GRPC_REQUEST_TIMEOUT", 2*time.Second, 100*time.Millisecond, 10*time.Second,
		)
		if err != nil {
			return Config{}, err
		}
		grpcShutdownTimeout, err = boundedDuration(
			lookup, "GRPC_SHUTDOWN_TIMEOUT", 10*time.Second, time.Second, 30*time.Second,
		)
		if err != nil {
			return Config{}, err
		}
	} else {
		identityTarget = valueOrDefault(lookup, "IDENTITY_GRPC_TARGET", "127.0.0.1:9091")
		if err := validateNetworkAddress("IDENTITY_GRPC_TARGET", identityTarget); err != nil {
			return Config{}, err
		}
		grpcCallTimeout, err = boundedDuration(
			lookup, "GRPC_CALL_TIMEOUT", time.Second, 100*time.Millisecond, 5*time.Second,
		)
		if err != nil {
			return Config{}, err
		}
	}
	grpcTLSCertFile := valueOrDefault(lookup, "GRPC_TLS_CERT_FILE", "")
	grpcTLSKeyFile := valueOrDefault(lookup, "GRPC_TLS_KEY_FILE", "")
	grpcTLSCAFile := valueOrDefault(lookup, "GRPC_TLS_CA_FILE", "")
	var grpcTLSServerName string
	var grpcTLSAllowedClientURIs []string
	if isIdentityService {
		grpcTLSAllowedClientURIs, err = allowedSPIFFEURIs(valueOrDefault(lookup, "GRPC_TLS_ALLOWED_CLIENT_URIS", ""))
		if err != nil {
			return Config{}, err
		}
	} else {
		grpcTLSServerName = valueOrDefault(lookup, "GRPC_TLS_SERVER_NAME", "")
	}
	grpcTLSConfigured := grpcTLSCertFile != "" || grpcTLSKeyFile != "" || grpcTLSCAFile != ""
	if grpcTLSConfigured && (grpcTLSCertFile == "" || grpcTLSKeyFile == "" || grpcTLSCAFile == "") {
		return Config{}, fmt.Errorf("GRPC_TLS_CERT_FILE, GRPC_TLS_KEY_FILE, and GRPC_TLS_CA_FILE must be configured together")
	}
	if environment == "production" && !grpcTLSConfigured {
		return Config{}, fmt.Errorf("gRPC mutual TLS certificate, key, and CA are required in production")
	}
	if !isIdentityService && grpcTLSConfigured && grpcTLSServerName == "" {
		return Config{}, fmt.Errorf("GRPC_TLS_SERVER_NAME is required for a TLS identity client")
	}
	if isIdentityService && grpcTLSConfigured && len(grpcTLSAllowedClientURIs) == 0 {
		return Config{}, fmt.Errorf("GRPC_TLS_ALLOWED_CLIENT_URIS is required for the identity gRPC server")
	}
	if !grpcTLSConfigured {
		plaintextAddress := identityTarget
		plaintextName := "IDENTITY_GRPC_TARGET"
		if isIdentityService {
			plaintextAddress = grpcAddress
			plaintextName = "GRPC_ADDR"
		}
		if !isLoopbackAddress(plaintextAddress) {
			return Config{}, fmt.Errorf("%s must be loopback when gRPC TLS is disabled", plaintextName)
		}
	}

	readHeaderTimeout, err := positiveDuration(lookup, "HTTP_READ_HEADER_TIMEOUT", 5*time.Second)
	if err != nil {
		return Config{}, err
	}
	readTimeout, err := positiveDuration(lookup, "HTTP_READ_TIMEOUT", 10*time.Second)
	if err != nil {
		return Config{}, err
	}
	writeTimeout, err := positiveDuration(lookup, "HTTP_WRITE_TIMEOUT", 20*time.Second)
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
	if !isIdentityService && grpcCallTimeout >= writeTimeout {
		return Config{}, fmt.Errorf("GRPC_CALL_TIMEOUT must be shorter than HTTP_WRITE_TIMEOUT")
	}

	databaseConfig, err := loadDatabaseConfig(lookup, databaseURL, writeTimeout, isOrderService || isPaymentService)
	if err != nil {
		return Config{}, err
	}
	var productCacheConfig ProductCacheConfig
	if isOrderService {
		productCacheConfig, err = loadProductCacheConfig(lookup, environment)
		if err != nil {
			return Config{}, err
		}
	}
	var jobClientConfig JobClientConfig
	if isIdentityService {
		jobClientConfig, err = loadJobClientConfig(lookup, environment)
		if err != nil {
			return Config{}, err
		}
	}
	requestBudget := readTimeout + databaseConfig.OperationTimeout + 500*time.Millisecond
	if !isIdentityService {
		requestBudget += grpcCallTimeout + databaseConfig.CommitResolutionTimeout
	}
	if productCacheConfig.Enabled {
		requestBudget += 2 * productCacheConfig.OperationTimeout
	}
	if jobClientConfig.Enabled {
		requestBudget += jobClientConfig.EnqueueTimeout
	}
	if requestBudget >= writeTimeout {
		return Config{}, fmt.Errorf("request read, gRPC, database, Redis, enqueue, and response budgets must fit within HTTP_WRITE_TIMEOUT")
	}

	var authConfig AuthConfig
	if isIdentityService {
		authConfig, err = loadAuthConfig(lookup)
		if err != nil {
			return Config{}, err
		}
	} else if key, configured := lookup("PASETO_V4_LOCAL_KEY"); configured && strings.TrimSpace(key) != "" {
		return Config{}, fmt.Errorf("PASETO_V4_LOCAL_KEY must not be configured outside user-service")
	}
	var commerceConfig CommerceConfig
	if isOrderService {
		rateLimit, err := loadRateLimitConfig(lookup, "COMMERCE", 20, 40, 1000)
		if err != nil {
			return Config{}, err
		}
		commerceConfig.RateLimit = rateLimit
	}
	var paymentConfig CommerceConfig
	if isPaymentService {
		rateLimit, err := loadRateLimitConfig(lookup, "PAYMENT", 10, 20, 1000)
		if err != nil {
			return Config{}, err
		}
		paymentConfig.RateLimit = rateLimit
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
		GRPC: GRPCConfig{
			Address: grpcAddress, IdentityTarget: identityTarget,
			CallTimeout: grpcCallTimeout, RequestTimeout: grpcRequestTimeout,
			ShutdownTimeout: grpcShutdownTimeout,
			TLSCertFile:     grpcTLSCertFile, TLSKeyFile: grpcTLSKeyFile,
			TLSCAFile: grpcTLSCAFile, TLSServerName: grpcTLSServerName,
			TLSAllowedClientURIs: grpcTLSAllowedClientURIs,
		},
		Database: databaseConfig,
		Log:      LogConfig{Level: logLevel},
		Auth:     authConfig, Commerce: commerceConfig, Payment: paymentConfig,
		ProductCache: productCacheConfig, Jobs: jobClientConfig,
	}, nil
}

func loadDatabaseConfig(
	lookup lookupEnv,
	databaseURL string,
	writeTimeout time.Duration,
	includeCommitResolution bool,
) (DatabaseConfig, error) {
	maxConns, err := integer(lookup, "DB_MAX_CONNS", 20, 1, 200)
	if err != nil {
		return DatabaseConfig{}, err
	}
	minIdleConns, err := integer(lookup, "DB_MIN_IDLE_CONNS", 2, 0, 200)
	if err != nil {
		return DatabaseConfig{}, err
	}
	if minIdleConns > maxConns {
		return DatabaseConfig{}, fmt.Errorf("DB_MIN_IDLE_CONNS must not exceed DB_MAX_CONNS")
	}
	maxConnLifetime, err := positiveDuration(lookup, "DB_MAX_CONN_LIFETIME", time.Hour)
	if err != nil {
		return DatabaseConfig{}, err
	}
	maxConnLifetimeJitter, err := nonNegativeDuration(lookup, "DB_MAX_CONN_LIFETIME_JITTER", 5*time.Minute)
	if err != nil {
		return DatabaseConfig{}, err
	}
	if maxConnLifetimeJitter > maxConnLifetime {
		return DatabaseConfig{}, fmt.Errorf("DB_MAX_CONN_LIFETIME_JITTER must not exceed DB_MAX_CONN_LIFETIME")
	}
	maxConnIdleTime, err := positiveDuration(lookup, "DB_MAX_CONN_IDLE_TIME", 30*time.Minute)
	if err != nil {
		return DatabaseConfig{}, err
	}
	healthCheckPeriod, err := positiveDuration(lookup, "DB_HEALTH_CHECK_PERIOD", time.Minute)
	if err != nil {
		return DatabaseConfig{}, err
	}
	pingTimeout, err := positiveDuration(lookup, "DB_PING_TIMEOUT", 2*time.Second)
	if err != nil {
		return DatabaseConfig{}, err
	}
	if writeTimeout > 0 && pingTimeout > writeTimeout {
		return DatabaseConfig{}, fmt.Errorf("DB_PING_TIMEOUT must not exceed HTTP_WRITE_TIMEOUT")
	}
	operationTimeout, err := boundedDuration(lookup, "DB_OPERATION_TIMEOUT", 5*time.Second, 500*time.Millisecond, 30*time.Second)
	if err != nil {
		return DatabaseConfig{}, err
	}
	if writeTimeout > 0 && operationTimeout >= writeTimeout {
		return DatabaseConfig{}, fmt.Errorf("DB_OPERATION_TIMEOUT must be shorter than HTTP_WRITE_TIMEOUT")
	}
	lockTimeout, err := boundedDuration(lookup, "DB_LOCK_TIMEOUT", 2*time.Second, 100*time.Millisecond, 10*time.Second)
	if err != nil {
		return DatabaseConfig{}, err
	}
	if lockTimeout >= operationTimeout {
		return DatabaseConfig{}, fmt.Errorf("DB_LOCK_TIMEOUT must be shorter than DB_OPERATION_TIMEOUT")
	}
	var commitResolutionTimeout time.Duration
	if includeCommitResolution {
		commitResolutionTimeout, err = boundedDuration(
			lookup, "DB_COMMIT_RESOLUTION_TIMEOUT", 2*time.Second, 100*time.Millisecond, 5*time.Second,
		)
		if err != nil {
			return DatabaseConfig{}, err
		}
		if writeTimeout > 0 && operationTimeout+commitResolutionTimeout >= writeTimeout {
			return DatabaseConfig{}, fmt.Errorf("DB_OPERATION_TIMEOUT plus DB_COMMIT_RESOLUTION_TIMEOUT must be shorter than HTTP_WRITE_TIMEOUT")
		}
	}
	return DatabaseConfig{
		URL: databaseURL, MaxConns: maxConns, MinIdleConns: minIdleConns,
		MaxConnLifetime: maxConnLifetime, MaxConnLifetimeJitter: maxConnLifetimeJitter,
		MaxConnIdleTime: maxConnIdleTime, HealthCheckPeriod: healthCheckPeriod,
		PingTimeout: pingTimeout, OperationTimeout: operationTimeout,
		LockTimeout: lockTimeout, CommitResolutionTimeout: commitResolutionTimeout,
	}, nil
}

func loadAuthConfig(lookup lookupEnv) (AuthConfig, error) {
	pasetoKey, ok := lookup("PASETO_V4_LOCAL_KEY")
	pasetoKey = strings.TrimSpace(pasetoKey)
	decodedKey, keyErr := hex.DecodeString(pasetoKey)
	if !ok || keyErr != nil || len(decodedKey) != 32 {
		return AuthConfig{}, fmt.Errorf("PASETO_V4_LOCAL_KEY must be a 32-byte hex key")
	}
	issuer := valueOrDefault(lookup, "TOKEN_ISSUER", "distributed-commerce/user-service")
	if issuer == "" || len(issuer) > 128 || strings.ContainsAny(issuer, "\r\n\t") {
		return AuthConfig{}, fmt.Errorf("TOKEN_ISSUER must be between 1 and 128 characters")
	}
	accessTokenTTL, err := boundedDuration(lookup, "ACCESS_TOKEN_TTL", 15*time.Minute, time.Minute, time.Hour)
	if err != nil {
		return AuthConfig{}, err
	}
	refreshTokenTTL, err := boundedDuration(lookup, "REFRESH_TOKEN_TTL", 7*24*time.Hour, time.Hour, 90*24*time.Hour)
	if err != nil {
		return AuthConfig{}, err
	}
	if refreshTokenTTL <= accessTokenTTL {
		return AuthConfig{}, fmt.Errorf("REFRESH_TOKEN_TTL must exceed ACCESS_TOKEN_TTL")
	}
	clockSkew, err := boundedNonNegativeDuration(lookup, "TOKEN_CLOCK_SKEW", 30*time.Second, 5*time.Minute)
	if err != nil {
		return AuthConfig{}, err
	}
	refreshReuseGrace, err := boundedNonNegativeDuration(lookup, "REFRESH_REUSE_GRACE", 5*time.Second, time.Minute)
	if err != nil {
		return AuthConfig{}, err
	}
	argon2Memory, err := integer(lookup, "ARGON2_MEMORY_KIB", 64*1024, 19*1024, 256*1024)
	if err != nil {
		return AuthConfig{}, err
	}
	argon2Iterations, err := integer(lookup, "ARGON2_ITERATIONS", 3, 1, 10)
	if err != nil {
		return AuthConfig{}, err
	}
	argon2Parallelism, err := integer(lookup, "ARGON2_PARALLELISM", 2, 1, 16)
	if err != nil {
		return AuthConfig{}, err
	}
	argon2MaxConcurrency, err := integer(lookup, "ARGON2_MAX_CONCURRENCY", 2, 1, 32)
	if err != nil {
		return AuthConfig{}, err
	}
	rateLimit, err := loadRateLimitConfig(lookup, "AUTH", 2, 5, 100)
	if err != nil {
		return AuthConfig{}, err
	}
	return AuthConfig{
		PasetoV4LocalKey: pasetoKey, Issuer: issuer,
		AccessTokenTTL: accessTokenTTL, RefreshTokenTTL: refreshTokenTTL,
		ClockSkew: clockSkew, RefreshReuseGrace: refreshReuseGrace,
		Argon2MemoryKiB: uint32(argon2Memory), Argon2Iterations: uint32(argon2Iterations),
		Argon2Parallelism: uint8(argon2Parallelism), Argon2MaxConcurrency: int(argon2MaxConcurrency),
		RateLimit: rateLimit,
	}, nil
}

func loadRateLimitConfig(
	lookup lookupEnv,
	prefix string,
	defaultRPS float64,
	defaultBurst, maximumBurst int32,
) (RateLimitConfig, error) {
	rps, err := decimal(lookup, prefix+"_RATE_LIMIT_RPS", defaultRPS, 0.1, 1000)
	if err != nil {
		return RateLimitConfig{}, err
	}
	burst, err := integer(lookup, prefix+"_RATE_LIMIT_BURST", defaultBurst, 1, maximumBurst)
	if err != nil {
		return RateLimitConfig{}, err
	}
	entryTTL, err := boundedDuration(
		lookup, prefix+"_RATE_LIMIT_ENTRY_TTL", 10*time.Minute, time.Minute, time.Hour,
	)
	if err != nil {
		return RateLimitConfig{}, err
	}
	maxEntries, err := integer(lookup, prefix+"_RATE_LIMIT_MAX_ENTRIES", 10000, 100, 100000)
	if err != nil {
		return RateLimitConfig{}, err
	}
	return RateLimitConfig{
		RequestsPerSecond: rps, Burst: int(burst), EntryTTL: entryTTL, MaxEntries: int(maxEntries),
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
	return validateNetworkAddress("HTTP_ADDR", address)
}

func validateNetworkAddress(name, address string) error {
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("%s must be a host:port address", name)
	}

	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return fmt.Errorf("%s must contain a numeric port between 1 and 65535", name)
	}
	return nil
}

func isLoopbackAddress(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func allowedSPIFFEURIs(raw string) ([]string, error) {
	if raw == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	if len(parts) > 16 {
		return nil, fmt.Errorf("GRPC_TLS_ALLOWED_CLIENT_URIS must contain at most 16 values")
	}
	seen := make(map[string]struct{}, len(parts))
	values := make([]string, 0, len(parts))
	for _, part := range parts {
		value := strings.TrimSpace(part)
		parsed, err := url.Parse(value)
		if err != nil || !canonicalSPIFFEURI(parsed, value) {
			return nil, fmt.Errorf("GRPC_TLS_ALLOWED_CLIENT_URIS must contain canonical SPIFFE URIs")
		}
		if _, duplicate := seen[value]; duplicate {
			return nil, fmt.Errorf("GRPC_TLS_ALLOWED_CLIENT_URIS must not contain duplicates")
		}
		seen[value] = struct{}{}
		values = append(values, value)
	}
	return values, nil
}

func canonicalSPIFFEURI(parsed *url.URL, value string) bool {
	if parsed.Scheme != "spiffe" || parsed.Opaque != "" || parsed.User != nil ||
		parsed.Host == "" || parsed.Host != parsed.Hostname() || !validSPIFFETrustDomain(parsed.Host) ||
		parsed.RawPath != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" ||
		parsed.Path == "" || parsed.Path == "/" || parsed.String() != value {
		return false
	}
	for _, segment := range strings.Split(strings.TrimPrefix(parsed.Path, "/"), "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
		for index := range len(segment) {
			character := segment[index]
			if !asciiLetterOrDigit(character) && character != '-' && character != '_' && character != '.' {
				return false
			}
		}
	}
	return true
}

func validSPIFFETrustDomain(value string) bool {
	if len(value) == 0 || len(value) > 255 || value != strings.ToLower(value) {
		return false
	}
	for index := range len(value) {
		character := value[index]
		if !asciiLetterOrDigit(character) && character != '.' && character != '-' && character != '_' {
			return false
		}
	}
	return true
}

func asciiLetterOrDigit(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9'
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
