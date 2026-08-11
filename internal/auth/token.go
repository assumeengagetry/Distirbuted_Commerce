package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"aidanwoods.dev/go-paseto"
	"github.com/google/uuid"
)

const (
	accessAudience         = "distributed-commerce-api"
	accessTokenType        = "access"
	maximumAccessTokenSize = 4096
	refreshTokenPrefix     = "rt1"
	refreshSecretLength    = 32
)

var (
	ErrInvalidAccessToken             = errors.New("invalid access token")
	ErrAccessTokenVerifierUnavailable = errors.New("access token verifier unavailable")
	ErrInvalidRefreshToken            = errors.New("invalid refresh token")
)

var accessImplicitAssertion = []byte("distributed-commerce/user-service/access/v1")

type Principal struct {
	UserID  uuid.UUID
	Role    string
	TokenID uuid.UUID
}

type TokenManager struct {
	key       paseto.V4SymmetricKey
	issuer    string
	accessTTL time.Duration
	clockSkew time.Duration
}

func NewTokenManager(keyHex, issuer string, accessTTL, clockSkew time.Duration) (*TokenManager, error) {
	key, err := paseto.V4SymmetricKeyFromHex(keyHex)
	if err != nil {
		return nil, fmt.Errorf("parse PASETO v4 local key: invalid 32-byte hex key")
	}
	issuer = strings.TrimSpace(issuer)
	if issuer == "" {
		return nil, fmt.Errorf("token issuer must not be empty")
	}
	if accessTTL <= 0 {
		return nil, fmt.Errorf("access token TTL must be positive")
	}
	if clockSkew < 0 || clockSkew > 5*time.Minute {
		return nil, fmt.Errorf("token clock skew must be between 0 and 5 minutes")
	}
	return &TokenManager{key: key, issuer: issuer, accessTTL: accessTTL, clockSkew: clockSkew}, nil
}

func (m *TokenManager) IssueAccess(userID uuid.UUID, role string, now time.Time) (string, time.Time, error) {
	if userID == uuid.Nil || !validRole(role) {
		return "", time.Time{}, fmt.Errorf("issue access token: invalid principal")
	}
	tokenID, err := uuid.NewRandom()
	if err != nil {
		return "", time.Time{}, fmt.Errorf("generate access token ID: %w", err)
	}

	now = now.UTC()
	expiresAt := now.Add(m.accessTTL)
	token := paseto.NewToken()
	token.SetIssuer(m.issuer)
	token.SetAudience(accessAudience)
	token.SetSubject(userID.String())
	token.SetJti(tokenID.String())
	token.SetIssuedAt(now)
	token.SetNotBefore(now)
	token.SetExpiration(expiresAt)
	token.SetString("role", role)
	token.SetString("token_type", accessTokenType)

	return token.V4Encrypt(m.key, accessImplicitAssertion), expiresAt, nil
}

func (m *TokenManager) ParseAccess(encoded string, now time.Time) (Principal, error) {
	if !ValidAccessTokenLength(encoded) {
		return Principal{}, ErrInvalidAccessToken
	}

	parser := paseto.MakeParser([]paseto.Rule{
		paseto.IssuedBy(m.issuer),
		paseto.ForAudience(accessAudience),
		m.validAccessTime(now.UTC()),
		requireStringClaim("token_type", accessTokenType),
	})
	token, err := parser.ParseV4Local(m.key, encoded, accessImplicitAssertion)
	if err != nil {
		return Principal{}, ErrInvalidAccessToken
	}

	subject, err := token.GetSubject()
	if err != nil {
		return Principal{}, ErrInvalidAccessToken
	}
	userID, err := uuid.Parse(subject)
	if err != nil || userID == uuid.Nil {
		return Principal{}, ErrInvalidAccessToken
	}
	jti, err := token.GetJti()
	if err != nil {
		return Principal{}, ErrInvalidAccessToken
	}
	tokenID, err := uuid.Parse(jti)
	if err != nil || tokenID == uuid.Nil {
		return Principal{}, ErrInvalidAccessToken
	}
	role, err := token.GetString("role")
	if err != nil || !validRole(role) {
		return Principal{}, ErrInvalidAccessToken
	}

	return Principal{UserID: userID, Role: role, TokenID: tokenID}, nil
}

func ValidAccessTokenLength(encoded string) bool {
	return len(encoded) > 0 && len(encoded) <= maximumAccessTokenSize && utf8.ValidString(encoded)
}

func (m *TokenManager) VerifyAccess(ctx context.Context, encoded string) (Principal, error) {
	if err := ctx.Err(); err != nil {
		return Principal{}, fmt.Errorf("%w: %v", ErrAccessTokenVerifierUnavailable, err)
	}
	return m.ParseAccess(encoded, time.Now())
}

func (m *TokenManager) validAccessTime(now time.Time) paseto.Rule {
	return func(token paseto.Token) error {
		issuedAt, err := token.GetIssuedAt()
		if err != nil {
			return ErrInvalidAccessToken
		}
		notBefore, err := token.GetNotBefore()
		if err != nil {
			return ErrInvalidAccessToken
		}
		expiresAt, err := token.GetExpiration()
		if err != nil {
			return ErrInvalidAccessToken
		}

		if issuedAt.After(now.Add(m.clockSkew)) || notBefore.After(now.Add(m.clockSkew)) {
			return ErrInvalidAccessToken
		}
		if !expiresAt.After(now.Add(-m.clockSkew)) || !expiresAt.After(issuedAt) {
			return ErrInvalidAccessToken
		}
		if expiresAt.Sub(issuedAt) > m.accessTTL+2*m.clockSkew {
			return ErrInvalidAccessToken
		}
		return nil
	}
}

func requireStringClaim(name, expected string) paseto.Rule {
	return func(token paseto.Token) error {
		value, err := token.GetString(name)
		if err != nil || value != expected {
			return ErrInvalidAccessToken
		}
		return nil
	}
}

type RefreshToken struct {
	ID   uuid.UUID
	Raw  string
	Hash []byte
}

func NewRefreshToken() (RefreshToken, error) {
	tokenID, err := uuid.NewRandom()
	if err != nil {
		return RefreshToken{}, fmt.Errorf("generate refresh token ID: %w", err)
	}
	secret := make([]byte, refreshSecretLength)
	if _, err := rand.Read(secret); err != nil {
		return RefreshToken{}, fmt.Errorf("generate refresh token secret: %w", err)
	}

	raw := refreshTokenPrefix + "." + tokenID.String() + "." + base64.RawURLEncoding.EncodeToString(secret)
	digest := sha256.Sum256([]byte(raw))
	return RefreshToken{ID: tokenID, Raw: raw, Hash: digest[:]}, nil
}

func ParseRefreshToken(raw string) (RefreshToken, error) {
	if len(raw) > 128 {
		return RefreshToken{}, ErrInvalidRefreshToken
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 || parts[0] != refreshTokenPrefix {
		return RefreshToken{}, ErrInvalidRefreshToken
	}
	tokenID, err := uuid.Parse(parts[1])
	if err != nil || tokenID == uuid.Nil {
		return RefreshToken{}, ErrInvalidRefreshToken
	}
	secret, err := base64.RawURLEncoding.Strict().DecodeString(parts[2])
	if err != nil || len(secret) != refreshSecretLength || base64.RawURLEncoding.EncodeToString(secret) != parts[2] {
		return RefreshToken{}, ErrInvalidRefreshToken
	}
	digest := sha256.Sum256([]byte(raw))
	return RefreshToken{ID: tokenID, Raw: raw, Hash: digest[:]}, nil
}

func validRole(role string) bool {
	return role == "customer" || role == "admin"
}
