package user

import (
	"time"

	"github.com/google/uuid"
)

type Role string

const (
	RoleCustomer Role = "customer"
	RoleAdmin    Role = "admin"
)

func (r Role) Valid() bool {
	return r == RoleCustomer || r == RoleAdmin
}

type Status string

const (
	StatusActive   Status = "active"
	StatusDisabled Status = "disabled"
)

type User struct {
	ID          uuid.UUID
	Email       string
	DisplayName string
	Role        Role
	Status      Status
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type Account struct {
	ID        uuid.UUID
	UserID    uuid.UUID
	Currency  string
	Balance   int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

type Profile struct {
	User    User
	Account Account
}

type Credentials struct {
	Profile      Profile
	PasswordHash string
}

type TokenPair struct {
	AccessToken      string
	RefreshToken     string
	AccessExpiresAt  time.Time
	RefreshExpiresAt time.Time
}

type AuthResult struct {
	Profile Profile
	Tokens  TokenPair
}
