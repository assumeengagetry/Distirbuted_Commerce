package main

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"

	commerceMigrations "github.com/assumeengagetry/distributed-commerce/db/migrations"
)

func main() {
	if err := run(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() (result error) {
	databaseURL := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if err := validateDatabaseURL(databaseURL); err != nil {
		return err
	}
	version, dirty, err := migrateDatabase(databaseURL)
	if err != nil {
		return fmt.Errorf("embedded database migration failed")
	}
	if dirty {
		return fmt.Errorf("database migration version %d is dirty", version)
	}
	_, _ = fmt.Fprintf(os.Stdout, "database migrated to version %d\n", version)
	return nil
}

func migrateDatabase(databaseURL string) (version uint, dirty bool, result error) {
	source, err := iofs.New(commerceMigrations.Files, ".")
	if err != nil {
		return 0, false, fmt.Errorf("load embedded migrations: %w", err)
	}
	migrator, err := migrate.NewWithSourceInstance("iofs", source, databaseURL)
	if err != nil {
		_ = source.Close()
		return 0, false, fmt.Errorf("initialize database migrator: %w", err)
	}
	defer func() {
		sourceErr, databaseErr := migrator.Close()
		result = errors.Join(result, sourceErr, databaseErr)
	}()
	if err := migrator.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return 0, false, fmt.Errorf("apply database migrations: %w", err)
	}
	version, dirty, err = migrator.Version()
	if err != nil {
		return 0, false, fmt.Errorf("read database migration version: %w", err)
	}
	return version, dirty, nil
}

func validateDatabaseURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Hostname() == "" || parsed.User == nil {
		return fmt.Errorf("DATABASE_URL must be a PostgreSQL URL with a host and user")
	}
	if parsed.Scheme != "postgres" && parsed.Scheme != "postgresql" {
		return fmt.Errorf("DATABASE_URL must use the postgres scheme")
	}
	if parsed.Query().Get("sslmode") != "verify-full" {
		return fmt.Errorf("DATABASE_URL must use sslmode=verify-full")
	}
	return nil
}
