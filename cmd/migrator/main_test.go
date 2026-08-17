package main

import "testing"

func TestValidateDatabaseURLRequiresVerifiedTLS(t *testing.T) {
	t.Parallel()
	if err := validateDatabaseURL("postgres://migrator@db.internal:5432/commerce?sslmode=verify-full"); err != nil {
		t.Fatalf("validateDatabaseURL(valid) error = %v", err)
	}
	for _, value := range []string{
		"",
		"postgres://migrator@/commerce?sslmode=verify-full",
		"postgres://db.internal/commerce?sslmode=verify-full",
		"postgres://migrator@db.internal/commerce?sslmode=require",
		"mysql://migrator@db.internal/commerce?sslmode=verify-full",
	} {
		if err := validateDatabaseURL(value); err == nil {
			t.Errorf("validateDatabaseURL(%q) error = nil", value)
		}
	}
}
