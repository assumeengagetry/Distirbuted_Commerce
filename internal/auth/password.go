package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

var (
	ErrInvalidPasswordHash = errors.New("invalid password hash")
	ErrPasswordHasherBusy  = errors.New("password hasher capacity reached")
)

type PasswordParams struct {
	MemoryKiB      uint32
	Iterations     uint32
	Parallelism    uint8
	SaltLength     uint32
	KeyLength      uint32
	MaxConcurrency int
}

type PasswordHasher struct {
	params    PasswordParams
	random    io.Reader
	semaphore chan struct{}
}

func NewPasswordHasher(params PasswordParams) (*PasswordHasher, error) {
	if err := validatePasswordParams(params); err != nil {
		return nil, err
	}
	return &PasswordHasher{
		params: params, random: rand.Reader, semaphore: make(chan struct{}, params.MaxConcurrency),
	}, nil
}

func (h *PasswordHasher) Hash(password string) (string, error) {
	if err := h.acquire(); err != nil {
		return "", err
	}
	defer h.release()

	salt := make([]byte, h.params.SaltLength)
	if _, err := io.ReadFull(h.random, salt); err != nil {
		return "", fmt.Errorf("generate password salt: %w", err)
	}

	digest := argon2.IDKey(
		[]byte(password),
		salt,
		h.params.Iterations,
		h.params.MemoryKiB,
		h.params.Parallelism,
		h.params.KeyLength,
	)

	return fmt.Sprintf(
		"$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version,
		h.params.MemoryKiB,
		h.params.Iterations,
		h.params.Parallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(digest),
	), nil
}

func (h *PasswordHasher) Verify(password, encodedHash string) (bool, error) {
	params, salt, expected, err := parsePasswordHash(encodedHash)
	if err != nil {
		return false, err
	}
	if err := h.acquire(); err != nil {
		return false, err
	}
	defer h.release()

	actual := argon2.IDKey(
		[]byte(password),
		salt,
		params.Iterations,
		params.MemoryKiB,
		params.Parallelism,
		uint32(len(expected)),
	)
	return subtle.ConstantTimeCompare(actual, expected) == 1, nil
}

func (h *PasswordHasher) acquire() error {
	select {
	case h.semaphore <- struct{}{}:
		return nil
	default:
		return ErrPasswordHasherBusy
	}
}

func (h *PasswordHasher) release() {
	<-h.semaphore
}

func parsePasswordHash(encodedHash string) (PasswordParams, []byte, []byte, error) {
	parts := strings.Split(encodedHash, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return PasswordParams{}, nil, nil, ErrInvalidPasswordHash
	}

	version, err := parseUintParameter(parts[2], "v=", 32)
	if err != nil || version != argon2.Version {
		return PasswordParams{}, nil, nil, ErrInvalidPasswordHash
	}

	parameterParts := strings.Split(parts[3], ",")
	if len(parameterParts) != 3 {
		return PasswordParams{}, nil, nil, ErrInvalidPasswordHash
	}
	memory, err := parseUintParameter(parameterParts[0], "m=", 32)
	if err != nil {
		return PasswordParams{}, nil, nil, ErrInvalidPasswordHash
	}
	iterations, err := parseUintParameter(parameterParts[1], "t=", 32)
	if err != nil {
		return PasswordParams{}, nil, nil, ErrInvalidPasswordHash
	}
	parallelism, err := parseUintParameter(parameterParts[2], "p=", 8)
	if err != nil {
		return PasswordParams{}, nil, nil, ErrInvalidPasswordHash
	}
	params := PasswordParams{
		MemoryKiB: uint32(memory), Iterations: uint32(iterations), Parallelism: uint8(parallelism), MaxConcurrency: 1,
	}

	salt, err := base64.RawStdEncoding.Strict().DecodeString(parts[4])
	if err != nil {
		return PasswordParams{}, nil, nil, ErrInvalidPasswordHash
	}
	expected, err := base64.RawStdEncoding.Strict().DecodeString(parts[5])
	if err != nil {
		return PasswordParams{}, nil, nil, ErrInvalidPasswordHash
	}
	params.SaltLength = uint32(len(salt))
	params.KeyLength = uint32(len(expected))
	if err := validatePasswordParams(params); err != nil {
		return PasswordParams{}, nil, nil, ErrInvalidPasswordHash
	}

	return params, salt, expected, nil
}

func parseUintParameter(value, prefix string, bitSize int) (uint64, error) {
	if !strings.HasPrefix(value, prefix) || len(value) == len(prefix) {
		return 0, ErrInvalidPasswordHash
	}
	parsed, err := strconv.ParseUint(strings.TrimPrefix(value, prefix), 10, bitSize)
	if err != nil {
		return 0, ErrInvalidPasswordHash
	}
	return parsed, nil
}

func validatePasswordParams(params PasswordParams) error {
	if params.MemoryKiB < 8*1024 || params.MemoryKiB > 256*1024 {
		return fmt.Errorf("Argon2 memory must be between 8192 and 262144 KiB")
	}
	if params.Iterations < 1 || params.Iterations > 10 {
		return fmt.Errorf("Argon2 iterations must be between 1 and 10")
	}
	if params.Parallelism < 1 || params.Parallelism > 16 {
		return fmt.Errorf("Argon2 parallelism must be between 1 and 16")
	}
	if params.SaltLength < 16 || params.SaltLength > 64 {
		return fmt.Errorf("Argon2 salt length must be between 16 and 64 bytes")
	}
	if params.KeyLength < 16 || params.KeyLength > 64 {
		return fmt.Errorf("Argon2 key length must be between 16 and 64 bytes")
	}
	if params.MaxConcurrency < 1 || params.MaxConcurrency > 32 {
		return fmt.Errorf("Argon2 maximum concurrency must be between 1 and 32")
	}
	return nil
}
