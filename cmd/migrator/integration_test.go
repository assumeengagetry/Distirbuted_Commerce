//go:build integration

package main

import (
	"os"
	"testing"
)

func TestEmbeddedMigrationsRunAgainstIntegrationDatabase(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	version, dirty, err := migrateDatabase(databaseURL)
	if err != nil {
		t.Fatalf("migrateDatabase() error = %v", err)
	}
	if dirty || version == 0 {
		t.Fatalf("embedded migration state = version:%d dirty:%t", version, dirty)
	}
}
