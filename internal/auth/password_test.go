package auth

import (
	"errors"
	"strings"
	"testing"
)

func TestPasswordHasherHashAndVerify(t *testing.T) {
	t.Parallel()

	hasher := testPasswordHasher(t)
	const password = "correct horse battery staple"
	hash, err := hasher.Hash(password)
	if err != nil {
		t.Fatalf("Hash() error = %v", err)
	}
	if !strings.HasPrefix(hash, "$argon2id$") {
		t.Fatalf("Hash() = %q, want Argon2id PHC string", hash)
	}
	if strings.Contains(hash, password) {
		t.Fatal("Hash() contains the plaintext password")
	}

	valid, err := hasher.Verify(password, hash)
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if !valid {
		t.Fatal("Verify() = false, want true")
	}
	valid, err = hasher.Verify("wrong password", hash)
	if err != nil {
		t.Fatalf("Verify(wrong) error = %v", err)
	}
	if valid {
		t.Fatal("Verify(wrong) = true, want false")
	}
}

func TestPasswordHasherUsesUniqueSalts(t *testing.T) {
	t.Parallel()

	hasher := testPasswordHasher(t)
	first, err := hasher.Hash("same-password-value")
	if err != nil {
		t.Fatalf("first Hash() error = %v", err)
	}
	second, err := hasher.Hash("same-password-value")
	if err != nil {
		t.Fatalf("second Hash() error = %v", err)
	}
	if first == second {
		t.Fatal("Hash() returned identical values for independently salted hashes")
	}
}

func TestPasswordHasherRejectsMalformedOrExpensiveHashes(t *testing.T) {
	t.Parallel()

	hasher := testPasswordHasher(t)
	tests := []string{
		"",
		"$argon2i$v=19$m=8192,t=1,p=1$c2FsdHNhbHRzYWx0c2FsdA$aGFzaGhhc2hoYXNoaGFzaGhhc2g",
		"$argon2id$v=18$m=8192,t=1,p=1$c2FsdHNhbHRzYWx0c2FsdA$aGFzaGhhc2hoYXNoaGFzaGhhc2g",
		"$argon2id$v=19extra$m=8192,t=1,p=1$c2FsdHNhbHRzYWx0c2FsdA$aGFzaGhhc2hoYXNoaGFzaGhhc2g",
		"$argon2id$v=19$m=999999,t=1,p=1$c2FsdHNhbHRzYWx0c2FsdA$aGFzaGhhc2hoYXNoaGFzaGhhc2g",
		"$argon2id$v=19$m=8192,t=1,p=1extra$c2FsdHNhbHRzYWx0c2FsdA$aGFzaGhhc2hoYXNoaGFzaGhhc2g",
		"$argon2id$v=19$m=8192,t=1,p=1$not-base64!$not-base64!",
	}

	for _, encoded := range tests {
		encoded := encoded
		t.Run(encoded, func(t *testing.T) {
			t.Parallel()
			if _, err := hasher.Verify("password", encoded); !errors.Is(err, ErrInvalidPasswordHash) {
				t.Fatalf("Verify() error = %v, want ErrInvalidPasswordHash", err)
			}
		})
	}
}

func TestNewPasswordHasherValidatesParameters(t *testing.T) {
	t.Parallel()

	tests := []PasswordParams{
		{MemoryKiB: 1024, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32, MaxConcurrency: 1},
		{MemoryKiB: 8192, Iterations: 0, Parallelism: 1, SaltLength: 16, KeyLength: 32, MaxConcurrency: 1},
		{MemoryKiB: 8192, Iterations: 1, Parallelism: 0, SaltLength: 16, KeyLength: 32, MaxConcurrency: 1},
		{MemoryKiB: 8192, Iterations: 1, Parallelism: 1, SaltLength: 8, KeyLength: 32, MaxConcurrency: 1},
		{MemoryKiB: 8192, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 8, MaxConcurrency: 1},
		{MemoryKiB: 8192, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32, MaxConcurrency: 0},
	}
	for _, params := range tests {
		if _, err := NewPasswordHasher(params); err == nil {
			t.Fatalf("NewPasswordHasher(%+v) error = nil, want error", params)
		}
	}
}

func TestPasswordHasherRejectsWorkWhenAtCapacity(t *testing.T) {
	t.Parallel()

	hasher, err := NewPasswordHasher(PasswordParams{
		MemoryKiB: 8192, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32, MaxConcurrency: 1,
	})
	if err != nil {
		t.Fatalf("NewPasswordHasher() error = %v", err)
	}
	hash, err := hasher.Hash("capacity-test-password")
	if err != nil {
		t.Fatalf("Hash() error = %v", err)
	}
	hasher.semaphore <- struct{}{}
	defer func() { <-hasher.semaphore }()
	if _, err := hasher.Hash("another-password-value"); !errors.Is(err, ErrPasswordHasherBusy) {
		t.Fatalf("Hash() error = %v, want ErrPasswordHasherBusy", err)
	}
	if _, err := hasher.Verify("capacity-test-password", hash); !errors.Is(err, ErrPasswordHasherBusy) {
		t.Fatalf("Verify() error = %v, want ErrPasswordHasherBusy", err)
	}
}

func testPasswordHasher(t *testing.T) *PasswordHasher {
	t.Helper()
	hasher, err := NewPasswordHasher(PasswordParams{
		MemoryKiB: 8192, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32, MaxConcurrency: 2,
	})
	if err != nil {
		t.Fatalf("NewPasswordHasher() error = %v", err)
	}
	return hasher
}
