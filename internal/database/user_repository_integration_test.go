//go:build integration

package database

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/assumeengagetry/distributed-commerce/internal/auth"
	"github.com/assumeengagetry/distributed-commerce/internal/observability"
	"github.com/assumeengagetry/distributed-commerce/internal/user"
)

func TestUserRepositoryRegistrationTransactionAndConstraints(t *testing.T) {
	ctx, pool, repository := openUserRepositoryIntegration(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	params, _ := integrationRegistration(t, now)

	profile, err := repository.CreateUser(ctx, params)
	if err != nil {
		t.Fatalf("CreateUser() error = %v", err)
	}
	t.Cleanup(func() { deleteIntegrationUser(t, pool, params.UserID) })
	if profile.User.Email != params.Email || profile.Account.Balance != 0 || profile.Account.Currency != "USD" {
		t.Errorf("profile = %+v", profile)
	}

	duplicate, _ := integrationRegistration(t, now.Add(time.Second))
	duplicate.Email = params.Email
	if _, err := repository.CreateUser(ctx, duplicate); !errors.Is(err, user.ErrEmailAlreadyExists) {
		t.Fatalf("duplicate CreateUser() error = %v, want ErrEmailAlreadyExists", err)
	}
	var candidateRows int
	if err := pool.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM users WHERE id = $1)
			+ (SELECT count(*) FROM accounts WHERE id = $2)
			+ (SELECT count(*) FROM auth_sessions WHERE id = $3)
	`, duplicate.UserID, duplicate.AccountID, duplicate.SessionID).Scan(&candidateRows); err != nil {
		t.Fatalf("count duplicate candidate rows: %v", err)
	}
	if candidateRows != 0 {
		t.Fatalf("duplicate registration left %d candidate rows", candidateRows)
	}

	lateFailure, _ := integrationRegistration(t, now.Add(2*time.Second))
	lateFailure.RefreshTokenHash = params.RefreshTokenHash
	if _, err := repository.CreateUser(ctx, lateFailure); err == nil {
		t.Fatal("registration with duplicate refresh digest succeeded")
	}
	if err := pool.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM users WHERE id = $1)
			+ (SELECT count(*) FROM accounts WHERE id = $2)
			+ (SELECT count(*) FROM auth_sessions WHERE id = $3)
	`, lateFailure.UserID, lateFailure.AccountID, lateFailure.SessionID).Scan(&candidateRows); err != nil {
		t.Fatalf("count late-failure candidate rows: %v", err)
	}
	if candidateRows != 0 {
		t.Fatalf("late transaction failure left %d candidate rows", candidateRows)
	}

	var users, accounts, sessions, tokens int
	if err := pool.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM users WHERE email = $1),
			(SELECT count(*) FROM accounts WHERE user_id = $2),
			(SELECT count(*) FROM auth_sessions WHERE user_id = $2),
			(SELECT count(*) FROM refresh_tokens rt JOIN auth_sessions s ON s.id = rt.session_id WHERE s.user_id = $2)
	`, params.Email, params.UserID).Scan(&users, &accounts, &sessions, &tokens); err != nil {
		t.Fatalf("count registration rows: %v", err)
	}
	if users != 1 || accounts != 1 || sessions != 1 || tokens != 1 {
		t.Fatalf("row counts = users:%d accounts:%d sessions:%d tokens:%d", users, accounts, sessions, tokens)
	}

	if _, err := pool.Exec(ctx, `UPDATE accounts SET balance = -1 WHERE user_id = $1`, params.UserID); err == nil {
		t.Fatal("negative balance update succeeded, want check constraint failure")
	}
}

func TestUserRepositoryConcurrentRefreshRotation(t *testing.T) {
	ctx, pool, repository := openUserRepositoryIntegration(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	params, current := integrationRegistration(t, now)
	if _, err := repository.CreateUser(ctx, params); err != nil {
		t.Fatalf("CreateUser() error = %v", err)
	}
	t.Cleanup(func() { deleteIntegrationUser(t, pool, params.UserID) })

	const contenders = 2
	start := make(chan struct{})
	type rotationOutcome struct {
		replacement auth.RefreshToken
		err         error
	}
	outcomes := make(chan rotationOutcome, contenders)
	var waitGroup sync.WaitGroup
	for range contenders {
		replacement, err := auth.NewRefreshToken()
		if err != nil {
			t.Fatalf("NewRefreshToken() error = %v", err)
		}
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			<-start
			_, err := repository.RotateRefreshToken(ctx, user.RotateRefreshTokenParams{
				CurrentTokenID: current.ID, CurrentTokenHash: current.Hash,
				ReplacementTokenID: replacement.ID, ReplacementHash: replacement.Hash,
				ReuseGrace: 5 * time.Second,
			})
			outcomes <- rotationOutcome{replacement: replacement, err: err}
		}()
	}
	close(start)
	waitGroup.Wait()
	close(outcomes)

	var successes, rejected int
	var winningReplacement auth.RefreshToken
	for outcome := range outcomes {
		err := outcome.err
		switch {
		case err == nil:
			successes++
			winningReplacement = outcome.replacement
		case errors.Is(err, user.ErrInvalidRefreshToken):
			rejected++
		default:
			t.Fatalf("RotateRefreshToken() unexpected error = %v", err)
		}
	}
	if successes != 1 || rejected != 1 {
		t.Fatalf("rotation results = %d successes, %d rejected", successes, rejected)
	}

	var activeTokens int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM refresh_tokens WHERE session_id = $1 AND consumed_at IS NULL`, params.SessionID).Scan(&activeTokens); err != nil {
		t.Fatalf("count active refresh tokens: %v", err)
	}
	if activeTokens != 1 {
		t.Fatalf("active refresh token count = %d, want 1", activeTokens)
	}
	var revoked bool
	if err := pool.QueryRow(ctx, `SELECT revoked_at IS NOT NULL FROM auth_sessions WHERE id = $1`, params.SessionID).Scan(&revoked); err != nil {
		t.Fatalf("query concurrent session revocation: %v", err)
	}
	if revoked {
		t.Fatal("concurrent refresh retry incorrectly revoked the session")
	}
	next, err := auth.NewRefreshToken()
	if err != nil {
		t.Fatalf("NewRefreshToken() error = %v", err)
	}
	if _, err := repository.RotateRefreshToken(ctx, user.RotateRefreshTokenParams{
		CurrentTokenID: winningReplacement.ID, CurrentTokenHash: winningReplacement.Hash,
		ReplacementTokenID: next.ID, ReplacementHash: next.Hash, ReuseGrace: 5 * time.Second,
	}); err != nil {
		t.Fatalf("winning replacement could not rotate: %v", err)
	}
}

func TestUserRepositoryRefreshLockTimeout(t *testing.T) {
	ctx, pool, _ := openUserRepositoryIntegration(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	params, current := integrationRegistration(t, now)
	repository := NewUserRepository(pool, 100*time.Millisecond, time.Second)
	if _, err := repository.CreateUser(ctx, params); err != nil {
		t.Fatalf("CreateUser() error = %v", err)
	}
	t.Cleanup(func() { deleteIntegrationUser(t, pool, params.UserID) })

	lockTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin lock transaction: %v", err)
	}
	defer func() { _ = lockTx.Rollback(context.Background()) }()
	if _, err := lockTx.Exec(ctx, `SELECT 1 FROM auth_sessions WHERE id = $1 FOR UPDATE`, params.SessionID); err != nil {
		t.Fatalf("lock auth session: %v", err)
	}
	replacement, err := auth.NewRefreshToken()
	if err != nil {
		t.Fatalf("NewRefreshToken() error = %v", err)
	}
	startedAt := time.Now()
	_, err = repository.RotateRefreshToken(ctx, user.RotateRefreshTokenParams{
		CurrentTokenID: current.ID, CurrentTokenHash: current.Hash,
		ReplacementTokenID: replacement.ID, ReplacementHash: replacement.Hash, ReuseGrace: 5 * time.Second,
	})
	if !errors.Is(err, user.ErrTemporarilyUnavailable) {
		t.Fatalf("RotateRefreshToken() error = %v, want ErrTemporarilyUnavailable", err)
	}
	if elapsed := time.Since(startedAt); elapsed > time.Second {
		t.Fatalf("lock timeout took %s, want less than 1s", elapsed)
	}
}

func TestUserRepositoryRefreshReplayRevokesSession(t *testing.T) {
	ctx, pool, repository := openUserRepositoryIntegration(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	params, current := integrationRegistration(t, now)
	if _, err := repository.CreateUser(ctx, params); err != nil {
		t.Fatalf("CreateUser() error = %v", err)
	}
	t.Cleanup(func() { deleteIntegrationUser(t, pool, params.UserID) })

	replacement, err := auth.NewRefreshToken()
	if err != nil {
		t.Fatalf("NewRefreshToken() error = %v", err)
	}
	if _, err := repository.RotateRefreshToken(ctx, user.RotateRefreshTokenParams{
		CurrentTokenID: current.ID, CurrentTokenHash: current.Hash,
		ReplacementTokenID: replacement.ID, ReplacementHash: replacement.Hash,
		ReuseGrace: 5 * time.Second,
	}); err != nil {
		t.Fatalf("first RotateRefreshToken() error = %v", err)
	}

	lateReplayReplacement, err := auth.NewRefreshToken()
	if err != nil {
		t.Fatalf("NewRefreshToken() error = %v", err)
	}
	_, err = repository.RotateRefreshToken(ctx, user.RotateRefreshTokenParams{
		CurrentTokenID: current.ID, CurrentTokenHash: current.Hash,
		ReplacementTokenID: lateReplayReplacement.ID, ReplacementHash: lateReplayReplacement.Hash,
		ReuseGrace: 0,
	})
	if !errors.Is(err, user.ErrInvalidRefreshToken) {
		t.Fatalf("replay RotateRefreshToken() error = %v, want ErrInvalidRefreshToken", err)
	}

	var revoked bool
	if err := pool.QueryRow(ctx, `SELECT revoked_at IS NOT NULL FROM auth_sessions WHERE id = $1`, params.SessionID).Scan(&revoked); err != nil {
		t.Fatalf("query session revocation: %v", err)
	}
	if !revoked {
		t.Fatal("replayed refresh token did not revoke the session")
	}

	next, err := auth.NewRefreshToken()
	if err != nil {
		t.Fatalf("NewRefreshToken() error = %v", err)
	}
	_, err = repository.RotateRefreshToken(ctx, user.RotateRefreshTokenParams{
		CurrentTokenID: replacement.ID, CurrentTokenHash: replacement.Hash,
		ReplacementTokenID: next.ID, ReplacementHash: next.Hash,
		ReuseGrace: 5 * time.Second,
	})
	if !errors.Is(err, user.ErrInvalidRefreshToken) {
		t.Fatalf("rotation after family revocation error = %v, want ErrInvalidRefreshToken", err)
	}
}

func TestUserRepositoryDeletesExpiredSessionsInBatches(t *testing.T) {
	ctx, pool, repository := openUserRepositoryIntegration(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	params, _ := integrationRegistration(t, now.Add(-2*time.Hour))
	params.SessionExpiresAt = now.Add(-time.Hour)
	if _, err := repository.CreateUser(ctx, params); err != nil {
		t.Fatalf("CreateUser() error = %v", err)
	}
	t.Cleanup(func() { deleteIntegrationUser(t, pool, params.UserID) })

	deleted, err := repository.DeleteExpiredSessions(ctx, 100)
	if err != nil {
		t.Fatalf("DeleteExpiredSessions() error = %v", err)
	}
	if deleted != 1 {
		t.Fatalf("DeleteExpiredSessions() = %d, want 1", deleted)
	}
	var users, sessions int
	if err := pool.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM users WHERE id = $1),
			(SELECT count(*) FROM auth_sessions WHERE user_id = $1)
	`, params.UserID).Scan(&users, &sessions); err != nil {
		t.Fatalf("count rows after session cleanup: %v", err)
	}
	if users != 1 || sessions != 0 {
		t.Fatalf("row counts after cleanup = users:%d sessions:%d", users, sessions)
	}
}

func openUserRepositoryIntegration(t *testing.T) (context.Context, *pgxpool.Pool, *UserRepository) {
	t.Helper()
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	pool, err := Open(ctx, databaseConfig(databaseURL), observability.NoopProviders())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(pool.Close)
	return ctx, pool, NewUserRepository(pool, 2*time.Second, 5*time.Second)
}

func integrationRegistration(t *testing.T, now time.Time) (user.CreateUserParams, auth.RefreshToken) {
	t.Helper()
	refreshToken, err := auth.NewRefreshToken()
	if err != nil {
		t.Fatalf("NewRefreshToken() error = %v", err)
	}
	unique := uuid.NewString()
	return user.CreateUserParams{
		UserID: uuid.New(), AccountID: uuid.New(), SessionID: uuid.New(), RefreshTokenID: refreshToken.ID,
		Email: fmt.Sprintf("integration-%s@example.com", unique), PasswordHash: "$argon2id$integration-test",
		DisplayName: "Integration User", Role: user.RoleCustomer, Currency: "USD", RefreshTokenHash: refreshToken.Hash,
		SessionExpiresAt: now.Add(24 * time.Hour), CreatedAt: now,
	}, refreshToken
}

func deleteIntegrationUser(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, userID); err != nil {
		t.Errorf("delete integration user: %v", err)
	}
}
