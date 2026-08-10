package user

import "errors"

var (
	ErrEmailAlreadyExists     = errors.New("email already registered")
	ErrInvalidCredentials     = errors.New("invalid credentials")
	ErrInvalidRefreshToken    = errors.New("invalid refresh token")
	ErrUserNotFound           = errors.New("user not found")
	ErrUserDisabled           = errors.New("user disabled")
	ErrInvalidEmail           = errors.New("invalid email")
	ErrInvalidPassword        = errors.New("password must be between 12 and 128 bytes")
	ErrInvalidDisplayName     = errors.New("display name must be between 1 and 100 characters")
	ErrTemporarilyUnavailable = errors.New("service temporarily unavailable")
)
