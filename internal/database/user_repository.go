package database

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	store "github.com/assumeengagetry/distributed-commerce/internal/database/sqlc"
	"github.com/assumeengagetry/distributed-commerce/internal/user"
)

type UserRepository struct {
	pool                  *pgxpool.Pool
	queries               *store.Queries
	lockTimeout           time.Duration
	statementTimeout      time.Duration
	securityCommitTimeout time.Duration
}

func NewUserRepository(pool *pgxpool.Pool, lockTimeout, statementTimeout time.Duration) *UserRepository {
	return &UserRepository{
		pool: pool, queries: store.New(pool),
		lockTimeout: lockTimeout, statementTimeout: statementTimeout, securityCommitTimeout: lockTimeout,
	}
}

func (r *UserRepository) CreateUser(ctx context.Context, params user.CreateUserParams) (user.Profile, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return user.Profile{}, fmt.Errorf("begin user registration transaction: %w", err)
	}
	defer r.rollback(tx)
	if err := r.configureTransaction(ctx, tx); err != nil {
		return user.Profile{}, err
	}

	queries := r.queries.WithTx(tx)
	dbUser, err := queries.CreateUser(ctx, store.CreateUserParams{
		ID:           params.UserID,
		Email:        params.Email,
		PasswordHash: params.PasswordHash,
		DisplayName:  params.DisplayName,
		Role:         string(params.Role),
		CreatedAt:    params.CreatedAt,
	})
	if err != nil {
		return user.Profile{}, mapUserWriteError(err)
	}

	dbAccount, err := queries.CreateAccount(ctx, store.CreateAccountParams{
		ID:        params.AccountID,
		UserID:    params.UserID,
		Currency:  params.Currency,
		CreatedAt: params.CreatedAt,
	})
	if err != nil {
		return user.Profile{}, fmt.Errorf("create account: %w", err)
	}

	if _, err := queries.CreateAuthSession(ctx, store.CreateAuthSessionParams{
		ID:        params.SessionID,
		UserID:    params.UserID,
		ExpiresAt: params.SessionExpiresAt,
		CreatedAt: params.CreatedAt,
	}); err != nil {
		return user.Profile{}, fmt.Errorf("create registration session: %w", err)
	}

	if _, err := queries.CreateRefreshToken(ctx, store.CreateRefreshTokenParams{
		ID:        params.RefreshTokenID,
		SessionID: params.SessionID,
		TokenHash: params.RefreshTokenHash,
		CreatedAt: params.CreatedAt,
	}); err != nil {
		return user.Profile{}, fmt.Errorf("create registration refresh token: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return user.Profile{}, fmt.Errorf("commit user registration: %w", err)
	}

	return profileFromModels(dbUser, dbAccount)
}

func (r *UserRepository) GetCredentialsByEmail(ctx context.Context, email string) (user.Credentials, error) {
	row, err := r.queries.GetUserCredentialsByEmail(ctx, email)
	if errors.Is(err, pgx.ErrNoRows) {
		return user.Credentials{}, user.ErrUserNotFound
	}
	if err != nil {
		return user.Credentials{}, fmt.Errorf("get user credentials: %w", err)
	}

	profile, err := profileFromCredentialRow(row)
	if err != nil {
		return user.Credentials{}, err
	}
	return user.Credentials{Profile: profile, PasswordHash: row.PasswordHash}, nil
}

func (r *UserRepository) CreateSession(ctx context.Context, params user.CreateSessionParams) error {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin session transaction: %w", err)
	}
	defer r.rollback(tx)
	if err := r.configureTransaction(ctx, tx); err != nil {
		return err
	}

	queries := r.queries.WithTx(tx)
	if _, err := queries.CreateAuthSession(ctx, store.CreateAuthSessionParams{
		ID:        params.SessionID,
		UserID:    params.UserID,
		ExpiresAt: params.SessionExpiresAt,
		CreatedAt: params.CreatedAt,
	}); err != nil {
		return fmt.Errorf("create auth session: %w", err)
	}
	if _, err := queries.CreateRefreshToken(ctx, store.CreateRefreshTokenParams{
		ID:        params.RefreshTokenID,
		SessionID: params.SessionID,
		TokenHash: params.RefreshTokenHash,
		CreatedAt: params.CreatedAt,
	}); err != nil {
		return fmt.Errorf("create refresh token: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit session: %w", err)
	}
	return nil
}

func (r *UserRepository) RotateRefreshToken(ctx context.Context, params user.RotateRefreshTokenParams) (user.RotationResult, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return user.RotationResult{}, fmt.Errorf("begin refresh rotation transaction: %w", err)
	}
	defer r.rollback(tx)
	if err := r.configureTransaction(ctx, tx); err != nil {
		return user.RotationResult{}, err
	}

	queries := r.queries.WithTx(tx)
	state, err := queries.GetRefreshTokenStateForUpdate(ctx, params.CurrentTokenID)
	if errors.Is(err, pgx.ErrNoRows) {
		return user.RotationResult{}, user.ErrInvalidRefreshToken
	}
	if err != nil {
		if isDatabaseTimeout(err) {
			return user.RotationResult{}, user.ErrTemporarilyUnavailable
		}
		return user.RotationResult{}, fmt.Errorf("lock refresh session: %w", err)
	}
	if subtle.ConstantTimeCompare(state.TokenHash, params.CurrentTokenHash) != 1 {
		return user.RotationResult{}, user.ErrInvalidRefreshToken
	}
	decisionCtx := ctx
	if state.ConsumedAt.Valid || state.Status != string(user.StatusActive) {
		securityCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.securityCommitTimeout)
		defer cancel()
		decisionCtx = securityCtx
	}
	databaseNow, err := databaseTime(decisionCtx, tx)
	if err != nil {
		return user.RotationResult{}, err
	}

	if state.SessionRevokedAt.Valid || !state.SessionExpiresAt.After(databaseNow) {
		return user.RotationResult{}, user.ErrInvalidRefreshToken
	}
	if state.Status != string(user.StatusActive) {
		return user.RotationResult{}, r.revokeAndCommit(decisionCtx, tx, queries, state.SessionID, databaseNow)
	}
	if state.ConsumedAt.Valid {
		if databaseNow.Sub(state.ConsumedAt.Time) > params.ReuseGrace {
			return user.RotationResult{}, r.revokeAndCommit(decisionCtx, tx, queries, state.SessionID, databaseNow)
		}
		return user.RotationResult{}, user.ErrInvalidRefreshToken
	}

	rows, err := queries.ConsumeRefreshToken(ctx, store.ConsumeRefreshTokenParams{
		ID:         params.CurrentTokenID,
		ConsumedAt: validTimestamptz(databaseNow),
	})
	if err != nil {
		return user.RotationResult{}, fmt.Errorf("consume refresh token: %w", err)
	}
	if rows != 1 {
		return user.RotationResult{}, fmt.Errorf("consume refresh token: expected one row, updated %d", rows)
	}

	if _, err := queries.CreateRefreshToken(ctx, store.CreateRefreshTokenParams{
		ID:        params.ReplacementTokenID,
		SessionID: state.SessionID,
		TokenHash: params.ReplacementHash,
		CreatedAt: databaseNow,
	}); err != nil {
		return user.RotationResult{}, fmt.Errorf("create replacement refresh token: %w", err)
	}

	profile, err := profileFromRefreshRow(state)
	if err != nil {
		return user.RotationResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return user.RotationResult{}, fmt.Errorf("commit refresh rotation: %w", err)
	}

	return user.RotationResult{Profile: profile, SessionExpiresAt: state.SessionExpiresAt, RotatedAt: databaseNow}, nil
}

func (r *UserRepository) RevokeSession(ctx context.Context, params user.RevokeSessionParams) error {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin session revocation transaction: %w", err)
	}
	defer r.rollback(tx)
	if err := r.configureTransaction(ctx, tx); err != nil {
		return err
	}

	queries := r.queries.WithTx(tx)
	state, err := queries.GetRefreshTokenStateForUpdate(ctx, params.RefreshTokenID)
	if errors.Is(err, pgx.ErrNoRows) {
		return user.ErrInvalidRefreshToken
	}
	if err != nil {
		return fmt.Errorf("lock session for revocation: %w", err)
	}
	if subtle.ConstantTimeCompare(state.TokenHash, params.RefreshTokenHash) != 1 {
		return user.ErrInvalidRefreshToken
	}
	databaseNow, err := databaseTime(ctx, tx)
	if err != nil {
		return err
	}

	if err := queries.RevokeAuthSession(ctx, store.RevokeAuthSessionParams{
		ID:        state.SessionID,
		UpdatedAt: databaseNow,
	}); err != nil {
		return fmt.Errorf("revoke auth session: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit session revocation: %w", err)
	}
	return nil
}

func (r *UserRepository) GetProfile(ctx context.Context, userID uuid.UUID) (user.Profile, error) {
	row, err := r.queries.GetUserProfileByID(ctx, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return user.Profile{}, user.ErrUserNotFound
	}
	if err != nil {
		return user.Profile{}, fmt.Errorf("get user profile: %w", err)
	}
	return profileFromProfileRow(row)
}

func (r *UserRepository) DeleteExpiredSessions(ctx context.Context, batchSize int32) (int64, error) {
	deleted, err := r.queries.DeleteExpiredAuthSessions(ctx, batchSize)
	if err != nil {
		return 0, fmt.Errorf("delete expired auth sessions: %w", err)
	}
	return deleted, nil
}

func (r *UserRepository) revokeAndCommit(
	ctx context.Context,
	tx pgx.Tx,
	queries *store.Queries,
	sessionID uuid.UUID,
	now time.Time,
) error {
	securityCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.securityCommitTimeout)
	defer cancel()
	if err := queries.RevokeAuthSession(securityCtx, store.RevokeAuthSessionParams{
		ID:        sessionID,
		UpdatedAt: now,
	}); err != nil {
		return fmt.Errorf("revoke compromised auth session: %w", err)
	}
	if err := tx.Commit(securityCtx); err != nil {
		return fmt.Errorf("commit compromised session revocation: %w", err)
	}
	return user.ErrInvalidRefreshToken
}

func (r *UserRepository) configureTransaction(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(
		ctx,
		`SELECT set_config('lock_timeout', $1, true), set_config('statement_timeout', $2, true)`,
		postgresDuration(r.lockTimeout),
		postgresDuration(r.statementTimeout),
	)
	if err != nil {
		return fmt.Errorf("configure transaction timeouts: %w", err)
	}
	return nil
}

func (r *UserRepository) rollback(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}

func databaseTime(ctx context.Context, tx pgx.Tx) (time.Time, error) {
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return time.Time{}, fmt.Errorf("read database time: %w", err)
	}
	return now.UTC(), nil
}

func postgresDuration(duration time.Duration) string {
	return strconv.FormatInt(duration.Milliseconds(), 10) + "ms"
}

func isDatabaseTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == "55P03" || pgErr.Code == "57014" ||
		pgErr.Code == "40P01" || pgErr.Code == "40001")
}

func profileFromModels(dbUser store.User, account store.Account) (user.Profile, error) {
	role, status, err := validateRoleAndStatus(dbUser.Role, dbUser.Status)
	if err != nil {
		return user.Profile{}, err
	}
	return user.Profile{
		User: user.User{
			ID:          dbUser.ID,
			Email:       dbUser.Email,
			DisplayName: dbUser.DisplayName,
			Role:        role,
			Status:      status,
			CreatedAt:   dbUser.CreatedAt,
			UpdatedAt:   dbUser.UpdatedAt,
		},
		Account: user.Account{
			ID:        account.ID,
			UserID:    account.UserID,
			Currency:  account.Currency,
			Balance:   account.Balance,
			CreatedAt: account.CreatedAt,
			UpdatedAt: account.UpdatedAt,
		},
	}, nil
}

func profileFromCredentialRow(row store.GetUserCredentialsByEmailRow) (user.Profile, error) {
	role, status, err := validateRoleAndStatus(row.Role, row.Status)
	if err != nil {
		return user.Profile{}, err
	}
	return user.Profile{
		User: user.User{
			ID: row.ID, Email: row.Email, DisplayName: row.DisplayName, Role: role, Status: status,
			CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
		},
		Account: user.Account{
			ID: row.AccountID, UserID: row.ID, Currency: row.Currency, Balance: row.Balance,
			CreatedAt: row.AccountCreatedAt, UpdatedAt: row.AccountUpdatedAt,
		},
	}, nil
}

func profileFromProfileRow(row store.GetUserProfileByIDRow) (user.Profile, error) {
	role, status, err := validateRoleAndStatus(row.Role, row.Status)
	if err != nil {
		return user.Profile{}, err
	}
	return user.Profile{
		User: user.User{
			ID: row.ID, Email: row.Email, DisplayName: row.DisplayName, Role: role, Status: status,
			CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
		},
		Account: user.Account{
			ID: row.AccountID, UserID: row.ID, Currency: row.Currency, Balance: row.Balance,
			CreatedAt: row.AccountCreatedAt, UpdatedAt: row.AccountUpdatedAt,
		},
	}, nil
}

func profileFromRefreshRow(row store.GetRefreshTokenStateForUpdateRow) (user.Profile, error) {
	role, status, err := validateRoleAndStatus(row.Role, row.Status)
	if err != nil {
		return user.Profile{}, err
	}
	return user.Profile{
		User: user.User{
			ID: row.UserID, Email: row.Email, DisplayName: row.DisplayName, Role: role, Status: status,
			CreatedAt: row.UserCreatedAt, UpdatedAt: row.UserUpdatedAt,
		},
		Account: user.Account{
			ID: row.AccountID, UserID: row.UserID, Currency: row.Currency, Balance: row.Balance,
			CreatedAt: row.AccountCreatedAt, UpdatedAt: row.AccountUpdatedAt,
		},
	}, nil
}

func validateRoleAndStatus(roleValue, statusValue string) (user.Role, user.Status, error) {
	role := user.Role(roleValue)
	if !role.Valid() {
		return "", "", fmt.Errorf("database contains invalid user role")
	}
	status := user.Status(statusValue)
	if status != user.StatusActive && status != user.StatusDisabled {
		return "", "", fmt.Errorf("database contains invalid user status")
	}
	return role, status, nil
}

func mapUserWriteError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "users_email_unique" {
		return user.ErrEmailAlreadyExists
	}
	return fmt.Errorf("create user: %w", err)
}

func validTimestamptz(value time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: value, Valid: true}
}
