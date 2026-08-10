package auth

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

const testTokenKey = "abababababababababababababababababababababababababababababababab"

func TestTokenManagerIssueAndParseAccess(t *testing.T) {
	t.Parallel()

	manager := testTokenManager(t, testTokenKey)
	now := time.Date(2026, time.August, 10, 12, 0, 0, 0, time.UTC)
	userID := uuid.New()
	encoded, expiresAt, err := manager.IssueAccess(userID, "customer", now)
	if err != nil {
		t.Fatalf("IssueAccess() error = %v", err)
	}
	if !strings.HasPrefix(encoded, "v4.local.") {
		t.Fatalf("IssueAccess() = %q, want v4.local token", encoded)
	}
	if !expiresAt.Equal(now.Add(15 * time.Minute)) {
		t.Errorf("expiresAt = %s, want %s", expiresAt, now.Add(15*time.Minute))
	}

	principal, err := manager.ParseAccess(encoded, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("ParseAccess() error = %v", err)
	}
	if principal.UserID != userID || principal.Role != "customer" || principal.TokenID == uuid.Nil {
		t.Errorf("principal = %+v", principal)
	}
}

func TestTokenManagerRejectsInvalidAccessTokens(t *testing.T) {
	t.Parallel()

	manager := testTokenManager(t, testTokenKey)
	otherManager := testTokenManager(t, strings.Repeat("cd", 32))
	now := time.Date(2026, time.August, 10, 12, 0, 0, 0, time.UTC)
	token, _, err := manager.IssueAccess(uuid.New(), "admin", now)
	if err != nil {
		t.Fatalf("IssueAccess() error = %v", err)
	}
	tampered := token[:len(token)-1] + "x"

	tests := []struct {
		name    string
		manager *TokenManager
		token   string
		now     time.Time
	}{
		{name: "empty", manager: manager, token: "", now: now},
		{name: "oversized", manager: manager, token: strings.Repeat("a", maximumAccessTokenSize+1), now: now},
		{name: "tampered", manager: manager, token: tampered, now: now},
		{name: "wrong key", manager: otherManager, token: token, now: now},
		{name: "expired beyond skew", manager: manager, token: token, now: now.Add(16 * time.Minute)},
		{name: "issued in future", manager: manager, token: token, now: now.Add(-time.Minute)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := tt.manager.ParseAccess(tt.token, tt.now); !errors.Is(err, ErrInvalidAccessToken) {
				t.Fatalf("ParseAccess() error = %v, want ErrInvalidAccessToken", err)
			}
		})
	}
}

func TestTokenManagerAllowsConfiguredClockSkew(t *testing.T) {
	t.Parallel()

	manager := testTokenManager(t, testTokenKey)
	now := time.Date(2026, time.August, 10, 12, 0, 0, 0, time.UTC)
	token, expiresAt, err := manager.IssueAccess(uuid.New(), "customer", now)
	if err != nil {
		t.Fatalf("IssueAccess() error = %v", err)
	}
	if _, err := manager.ParseAccess(token, expiresAt.Add(15*time.Second)); err != nil {
		t.Fatalf("ParseAccess() within skew error = %v", err)
	}
}

func TestRefreshTokenRoundTrip(t *testing.T) {
	t.Parallel()

	first, err := NewRefreshToken()
	if err != nil {
		t.Fatalf("NewRefreshToken() error = %v", err)
	}
	second, err := NewRefreshToken()
	if err != nil {
		t.Fatalf("NewRefreshToken() second error = %v", err)
	}
	if first.Raw == second.Raw || bytes.Equal(first.Hash, second.Hash) {
		t.Fatal("independent refresh tokens are identical")
	}
	parsed, err := ParseRefreshToken(first.Raw)
	if err != nil {
		t.Fatalf("ParseRefreshToken() error = %v", err)
	}
	if parsed.ID != first.ID || !bytes.Equal(parsed.Hash, first.Hash) {
		t.Errorf("parsed token = %+v, want ID %s and matching digest", parsed, first.ID)
	}
	if bytes.Contains(first.Hash, []byte(first.Raw)) {
		t.Fatal("refresh token digest contains raw token")
	}
}

func TestParseRefreshTokenRejectsMalformedValues(t *testing.T) {
	t.Parallel()

	tests := []string{
		"",
		"rt2." + uuid.NewString() + ".secret",
		"rt1.not-a-uuid.secret",
		"rt1." + uuid.NewString() + ".short",
		"rt1." + uuid.NewString() + "." + strings.Repeat("!", 43),
		strings.Repeat("a", 129),
	}
	for _, raw := range tests {
		if _, err := ParseRefreshToken(raw); !errors.Is(err, ErrInvalidRefreshToken) {
			t.Fatalf("ParseRefreshToken(%q) error = %v, want ErrInvalidRefreshToken", raw, err)
		}
	}
}

func TestPrincipalContext(t *testing.T) {
	t.Parallel()

	principal := Principal{UserID: uuid.New(), Role: "customer", TokenID: uuid.New()}
	ctx := ContextWithPrincipal(t.Context(), principal)
	got, ok := PrincipalFromContext(ctx)
	if !ok || got != principal {
		t.Fatalf("PrincipalFromContext() = (%+v, %v), want (%+v, true)", got, ok, principal)
	}
}

func testTokenManager(t *testing.T, key string) *TokenManager {
	t.Helper()
	manager, err := NewTokenManager(key, "test-issuer", 15*time.Minute, 30*time.Second)
	if err != nil {
		t.Fatalf("NewTokenManager() error = %v", err)
	}
	return manager
}
