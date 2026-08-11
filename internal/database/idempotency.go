package database

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	store "github.com/assumeengagetry/distributed-commerce/internal/database/sqlc"
)

var (
	errIdempotencyConflict       = errors.New("idempotency key was used for a different request")
	errIdempotencyInProgress     = errors.New("idempotent operation is still in progress")
	errIdempotencyOutcomeUnknown = errors.New("idempotent operation outcome is unknown")
)

type idempotencyClaim struct {
	Replay     bool
	ResourceID uuid.UUID
}

func claimIdempotency(
	ctx context.Context,
	queries *store.Queries,
	actorID uuid.UUID,
	operation string,
	keyHash, requestHash []byte,
	resourceID uuid.UUID,
) (idempotencyClaim, error) {
	_, err := queries.ReserveIdempotencyKey(ctx, store.ReserveIdempotencyKeyParams{
		ActorID: actorID, Operation: operation, KeyHash: keyHash,
		RequestHash: requestHash, ResourceID: resourceID,
	})
	if err == nil {
		return idempotencyClaim{ResourceID: resourceID}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "55P03" {
			return idempotencyClaim{}, errIdempotencyInProgress
		}
		return idempotencyClaim{}, fmt.Errorf("reserve idempotency key: %w", err)
	}

	record, err := queries.GetIdempotencyKeyForUpdate(ctx, store.GetIdempotencyKeyForUpdateParams{
		ActorID: actorID, Operation: operation, KeyHash: keyHash,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return idempotencyClaim{}, errIdempotencyInProgress
	}
	if err != nil {
		return idempotencyClaim{}, fmt.Errorf("read idempotency key: %w", err)
	}
	if subtle.ConstantTimeCompare(record.RequestHash, requestHash) != 1 {
		return idempotencyClaim{}, errIdempotencyConflict
	}
	if record.State != "completed" || !record.ResponseStatus.Valid || record.ResponseStatus.Int16 != 201 ||
		!record.CompletedAt.Valid {
		return idempotencyClaim{}, errIdempotencyInProgress
	}
	return idempotencyClaim{Replay: true, ResourceID: record.ResourceID}, nil
}

func completeIdempotency(
	ctx context.Context,
	queries *store.Queries,
	actorID uuid.UUID,
	operation string,
	keyHash, requestHash []byte,
	resourceID uuid.UUID,
) error {
	_, err := queries.CompleteIdempotencyKey(ctx, store.CompleteIdempotencyKeyParams{
		ActorID: actorID, Operation: operation, KeyHash: keyHash, RequestHash: requestHash,
		ResourceID: resourceID, ResponseStatus: pgtype.Int2{Int16: 201, Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("complete idempotency key: reservation invariant violated")
	}
	if err != nil {
		return fmt.Errorf("complete idempotency key: %w", err)
	}
	return nil
}

func completedIdempotencyResource(
	ctx context.Context,
	queries *store.Queries,
	actorID uuid.UUID,
	operation string,
	keyHash, requestHash []byte,
) (uuid.UUID, error) {
	record, err := queries.GetIdempotencyKey(ctx, store.GetIdempotencyKeyParams{
		ActorID: actorID, Operation: operation, KeyHash: keyHash,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, errIdempotencyOutcomeUnknown
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("resolve idempotency key: %w", err)
	}
	if subtle.ConstantTimeCompare(record.RequestHash, requestHash) != 1 {
		return uuid.Nil, errIdempotencyConflict
	}
	if record.State != "completed" || !record.ResponseStatus.Valid || record.ResponseStatus.Int16 != 201 ||
		!record.CompletedAt.Valid {
		return uuid.Nil, errIdempotencyOutcomeUnknown
	}
	return record.ResourceID, nil
}

func waitForCompletedIdempotency(
	ctx context.Context,
	queries *store.Queries,
	actorID uuid.UUID,
	operation string,
	keyHash, requestHash []byte,
) (uuid.UUID, error) {
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		resourceID, err := completedIdempotencyResource(
			ctx, queries, actorID, operation, keyHash, requestHash,
		)
		if err == nil || errors.Is(err, errIdempotencyConflict) {
			return resourceID, err
		}
		select {
		case <-ctx.Done():
			return uuid.Nil, errIdempotencyOutcomeUnknown
		case <-ticker.C:
		}
	}
}

func commitOutcomeIsKnown(err error) bool {
	if err == nil {
		return true
	}
	if pgconn.SafeToRetry(err) || errors.Is(err, pgx.ErrTxCommitRollback) {
		return true
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	if pgErr.Code == "08007" || pgErr.Code == "40003" || strings.HasPrefix(pgErr.Code, "08") ||
		strings.EqualFold(pgErr.Severity, "FATAL") || strings.EqualFold(pgErr.Severity, "PANIC") {
		return false
	}
	return true
}
