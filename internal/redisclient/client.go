package redisclient

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"

	"github.com/redis/go-redis/v9"

	"github.com/assumeengagetry/distributed-commerce/internal/config"
)

func New(cfg config.RedisConfig) (*redis.Client, error) {
	if cfg.Address == "" || cfg.Username == "" || cfg.Password == "" {
		return nil, fmt.Errorf("Redis address, username, and password are required")
	}
	if cfg.Database < 0 || cfg.DialTimeout <= 0 || cfg.ReadTimeout <= 0 || cfg.WriteTimeout <= 0 ||
		cfg.PoolTimeout <= 0 || cfg.PoolSize <= 0 {
		return nil, fmt.Errorf("Redis database, timeouts, and pool size are invalid")
	}
	tlsConfig, err := clientTLSConfig(cfg)
	if err != nil {
		return nil, err
	}
	return redis.NewClient(&redis.Options{
		Addr: cfg.Address, Username: cfg.Username, Password: cfg.Password, DB: cfg.Database,
		DialTimeout: cfg.DialTimeout, ReadTimeout: cfg.ReadTimeout, WriteTimeout: cfg.WriteTimeout,
		PoolTimeout: cfg.PoolTimeout, PoolSize: cfg.PoolSize, MaxRetries: -1,
		ContextTimeoutEnabled: true, TLSConfig: tlsConfig,
	}), nil
}

func clientTLSConfig(cfg config.RedisConfig) (*tls.Config, error) {
	if cfg.TLSCAFile == "" {
		if cfg.TLSCertFile != "" || cfg.TLSKeyFile != "" || cfg.TLSServerName != "" {
			return nil, fmt.Errorf("Redis TLS configuration is incomplete")
		}
		return nil, nil
	}
	if cfg.TLSServerName == "" || (cfg.TLSCertFile == "") != (cfg.TLSKeyFile == "") {
		return nil, fmt.Errorf("Redis TLS CA, server name, and optional client certificate pair are required")
	}
	caPEM, err := os.ReadFile(cfg.TLSCAFile)
	if err != nil {
		return nil, fmt.Errorf("read Redis CA: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("Redis CA contains no certificates")
	}
	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS13, RootCAs: roots, ServerName: cfg.TLSServerName,
	}
	if cfg.TLSCertFile != "" {
		certificate, err := tls.LoadX509KeyPair(cfg.TLSCertFile, cfg.TLSKeyFile)
		if err != nil {
			return nil, fmt.Errorf("load Redis client certificate: %w", err)
		}
		tlsConfig.Certificates = []tls.Certificate{certificate}
	}
	return tlsConfig, nil
}
