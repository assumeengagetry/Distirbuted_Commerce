package database

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	store "github.com/assumeengagetry/distributed-commerce/internal/database/sqlc"
	"github.com/assumeengagetry/distributed-commerce/internal/idempotency"
	"github.com/assumeengagetry/distributed-commerce/internal/payment"
)

type PaymentRepository struct {
	pool                    *pgxpool.Pool
	queries                 *store.Queries
	lockTimeout             time.Duration
	statementTimeout        time.Duration
	commitResolutionTimeout time.Duration
	commitTransaction       func(context.Context, pgx.Tx) error
}

func NewPaymentRepository(
	pool *pgxpool.Pool,
	lockTimeout, statementTimeout, commitResolutionTimeout time.Duration,
) *PaymentRepository {
	return &PaymentRepository{
		pool: pool, queries: store.New(pool), lockTimeout: lockTimeout, statementTimeout: statementTimeout,
		commitResolutionTimeout: commitResolutionTimeout,
		commitTransaction:       func(ctx context.Context, tx pgx.Tx) error { return tx.Commit(ctx) },
	}
}

func (r *PaymentRepository) CreatePayment(
	ctx context.Context,
	actorID uuid.UUID,
	params payment.CreateParams,
) (payment.Payment, error) {
	tx, queries, err := r.begin(ctx, "payment creation")
	if err != nil {
		return payment.Payment{}, err
	}
	defer rollbackCommerceTransaction(tx)

	actor, err := queries.GetPaymentActorForShare(ctx, actorID)
	if errors.Is(err, pgx.ErrNoRows) {
		return payment.Payment{}, payment.ErrForbidden
	}
	if err != nil {
		return payment.Payment{}, mapPaymentDatabaseError("lock payment actor", err)
	}
	if err := validatePaymentActor(actor.Role, actor.Status, payment.RoleCustomer); err != nil {
		return payment.Payment{}, err
	}
	claim, err := claimIdempotency(
		ctx, queries, actorID, idempotency.PaymentCreateOperation,
		params.KeyHash, params.RequestHash, params.PaymentID,
	)
	if err != nil {
		return payment.Payment{}, mapPaymentIdempotencyError(err)
	}
	if claim.Replay {
		replayed, err := loadPayment(ctx, queries, actorID, claim.ResourceID)
		if err != nil {
			return payment.Payment{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return payment.Payment{}, mapPaymentDatabaseError("commit payment replay read", err)
		}
		replayed.IdempotencyReplay = true
		return replayed, nil
	}

	order, err := queries.GetOrderForPayment(ctx, store.GetOrderForPaymentParams{
		ID: params.OrderID, UserID: actorID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return payment.Payment{}, payment.ErrOrderNotFound
	}
	if err != nil {
		return payment.Payment{}, mapPaymentDatabaseError("lock order for payment", err)
	}
	switch order.Status {
	case "paid":
		return payment.Payment{}, payment.ErrOrderAlreadyPaid
	case "pending":
	default:
		return payment.Payment{}, payment.ErrOrderNotPayable
	}

	account, err := queries.GetAccountForPayment(ctx, actorID)
	if errors.Is(err, pgx.ErrNoRows) {
		return payment.Payment{}, fmt.Errorf("lock account for payment: customer account not found")
	}
	if err != nil {
		return payment.Payment{}, mapPaymentDatabaseError("lock account for payment", err)
	}
	if account.Currency != order.Currency {
		return payment.Payment{}, payment.ErrCurrencyMismatch
	}
	if account.Balance < order.TotalAmount {
		return payment.Payment{}, payment.ErrInsufficientFunds
	}
	now, err := databaseTime(ctx, tx)
	if err != nil {
		return payment.Payment{}, mapPaymentDatabaseError("read payment time", err)
	}
	debited, err := queries.DebitAccountForPayment(ctx, store.DebitAccountForPaymentParams{
		Amount: order.TotalAmount, UpdatedAt: now, AccountID: account.ID,
		UserID: actorID, Currency: account.Currency, ExpectedBalance: account.Balance,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return payment.Payment{}, payment.ErrInsufficientFunds
	}
	if err != nil {
		return payment.Payment{}, mapPaymentDatabaseError("debit account", err)
	}
	dbPayment, err := queries.CreatePayment(ctx, store.CreatePaymentParams{
		ID: params.PaymentID, OrderID: order.ID, UserID: actorID, AccountID: account.ID,
		AccountBalanceVersion: debited.BalanceVersion,
		Currency:              order.Currency, Amount: order.TotalAmount,
		BalanceBefore: account.Balance, BalanceAfter: debited.Balance, CreatedAt: now,
	})
	if err != nil {
		return payment.Payment{}, mapPaymentWriteError(err)
	}
	if _, err := queries.MarkOrderPaid(ctx, store.MarkOrderPaidParams{
		ID: order.ID, UserID: actorID, UpdatedAt: now,
	}); errors.Is(err, pgx.ErrNoRows) {
		return payment.Payment{}, payment.ErrOrderNotPayable
	} else if err != nil {
		return payment.Payment{}, mapPaymentDatabaseError("mark order paid", err)
	}
	if err := completeIdempotency(
		ctx, queries, actorID, idempotency.PaymentCreateOperation,
		params.KeyHash, params.RequestHash, params.PaymentID,
	); err != nil {
		return payment.Payment{}, mapPaymentDatabaseError("complete payment idempotency", err)
	}
	if err := r.commitTransaction(ctx, tx); err != nil {
		if commitOutcomeIsKnown(err) {
			return payment.Payment{}, mapPaymentDatabaseError("commit payment", err)
		}
		return r.resolvePaymentCommit(ctx, actorID, params)
	}
	return paymentFromModel(dbPayment), nil
}

func (r *PaymentRepository) GetPayment(
	ctx context.Context,
	actor payment.Actor,
	paymentID uuid.UUID,
) (payment.Payment, error) {
	tx, queries, err := r.begin(ctx, "payment read")
	if err != nil {
		return payment.Payment{}, err
	}
	defer rollbackCommerceTransaction(tx)
	storedActor, err := queries.GetPaymentActorForShare(ctx, actor.UserID)
	if errors.Is(err, pgx.ErrNoRows) {
		return payment.Payment{}, payment.ErrForbidden
	}
	if err != nil {
		return payment.Payment{}, mapPaymentDatabaseError("authorize payment read", err)
	}
	if err := validatePaymentActor(storedActor.Role, storedActor.Status, actor.Role); err != nil {
		return payment.Payment{}, err
	}
	var model store.Payment
	if actor.Role == payment.RoleAdmin {
		model, err = queries.GetPaymentByID(ctx, paymentID)
	} else {
		model, err = queries.GetPaymentByIDAndUser(ctx, store.GetPaymentByIDAndUserParams{
			ID: paymentID, UserID: actor.UserID,
		})
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return payment.Payment{}, payment.ErrPaymentNotFound
	}
	if err != nil {
		return payment.Payment{}, mapPaymentDatabaseError("get payment", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return payment.Payment{}, mapPaymentDatabaseError("commit payment read", err)
	}
	return paymentFromModel(model), nil
}

func (r *PaymentRepository) resolvePaymentCommit(
	ctx context.Context,
	actorID uuid.UUID,
	params payment.CreateParams,
) (payment.Payment, error) {
	resolutionCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.commitResolutionTimeout)
	defer cancel()
	resourceID, err := waitForCompletedIdempotency(
		resolutionCtx, r.queries, actorID, idempotency.PaymentCreateOperation,
		params.KeyHash, params.RequestHash,
	)
	if err != nil {
		return payment.Payment{}, mapPaymentIdempotencyError(err)
	}
	result, err := loadPayment(resolutionCtx, r.queries, actorID, resourceID)
	if err != nil {
		return payment.Payment{}, err
	}
	result.IdempotencyReplay = true
	return result, nil
}

func loadPayment(
	ctx context.Context,
	queries *store.Queries,
	actorID, paymentID uuid.UUID,
) (payment.Payment, error) {
	model, err := queries.GetPaymentByIDAndUser(ctx, store.GetPaymentByIDAndUserParams{
		ID: paymentID, UserID: actorID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return payment.Payment{}, fmt.Errorf("load idempotent payment: resource not found")
	}
	if err != nil {
		return payment.Payment{}, mapPaymentDatabaseError("load idempotent payment", err)
	}
	return paymentFromModel(model), nil
}

func (r *PaymentRepository) begin(ctx context.Context, operation string) (pgx.Tx, *store.Queries, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, nil, mapPaymentDatabaseError("begin "+operation, err)
	}
	if _, err := tx.Exec(
		ctx,
		`SELECT set_config('lock_timeout', $1, true), set_config('statement_timeout', $2, true)`,
		postgresDuration(r.lockTimeout), postgresDuration(r.statementTimeout),
	); err != nil {
		rollbackCommerceTransaction(tx)
		return nil, nil, mapPaymentDatabaseError("configure "+operation+" transaction", err)
	}
	return tx, r.queries.WithTx(tx), nil
}

func validatePaymentActor(role, status, requiredRole string) error {
	if status != "active" {
		return payment.ErrAccountDisabled
	}
	if role != requiredRole {
		return payment.ErrForbidden
	}
	return nil
}

func mapPaymentIdempotencyError(err error) error {
	switch {
	case errors.Is(err, errIdempotencyConflict):
		return payment.ErrIdempotencyConflict
	case errors.Is(err, errIdempotencyInProgress):
		return payment.ErrIdempotencyInProgress
	case errors.Is(err, errIdempotencyOutcomeUnknown):
		return payment.ErrOperationOutcomeUnknown
	default:
		return mapPaymentDatabaseError("payment idempotency", err)
	}
}

func mapPaymentWriteError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "payments_order_unique" {
		return payment.ErrOrderAlreadyPaid
	}
	return mapPaymentDatabaseError("create payment", err)
}

func mapPaymentDatabaseError(operation string, err error) error {
	if isDatabaseTimeout(err) {
		return payment.ErrTemporarilyUnavailable
	}
	return fmt.Errorf("%s: %w", operation, err)
}

func paymentFromModel(model store.Payment) payment.Payment {
	return payment.Payment{
		ID: model.ID, OrderID: model.OrderID, UserID: model.UserID, AccountID: model.AccountID,
		Status: payment.Status(model.Status), Currency: model.Currency, Amount: model.Amount,
		BalanceBefore: model.BalanceBefore, BalanceAfter: model.BalanceAfter, CreatedAt: model.CreatedAt,
	}
}
