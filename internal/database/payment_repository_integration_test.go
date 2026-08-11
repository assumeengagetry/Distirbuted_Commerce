//go:build integration

package database

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/assumeengagetry/distributed-commerce/internal/idempotency"
	commerce "github.com/assumeengagetry/distributed-commerce/internal/order"
	"github.com/assumeengagetry/distributed-commerce/internal/payment"
)

func TestPaymentRepositoryAtomicDebitReplayAndOwnership(t *testing.T) {
	ctx, pool, orderRepository := openOrderRepositoryIntegration(t)
	paymentRepository := NewPaymentRepository(pool, 5*time.Second, 10*time.Second, 2*time.Second)
	adminID := createCommerceActor(t, ctx, pool, commerce.RoleAdmin)
	customerID := createCommerceActor(t, ctx, pool, commerce.RoleCustomer)
	otherCustomerID := createCommerceActor(t, ctx, pool, commerce.RoleCustomer)
	productID := uuid.New()
	userIDs := []uuid.UUID{adminID, customerID, otherCustomerID}
	registerCommerceCleanup(t, pool, userIDs, []uuid.UUID{productID})
	registerPaymentCleanup(t, pool, userIDs)
	createIntegrationProduct(t, ctx, orderRepository, adminID, productID, uniqueIntegrationSKU("PAYMENT"), 3000, 1)
	setAccountBalance(t, ctx, pool, customerID, 5000)
	createdOrder, err := orderRepository.CreateOrder(
		ctx, customerID, idempotentOrderParams(productID, 1, "payment-order", "payment-order"),
	)
	if err != nil {
		t.Fatalf("create order for payment: %v", err)
	}
	if _, err := paymentRepository.CreatePayment(
		ctx, otherCustomerID, integrationPaymentParams(createdOrder.ID, "cross-owner-payment", "cross-owner-payment"),
	); !errors.Is(err, payment.ErrOrderNotFound) {
		t.Fatalf("cross-owner CreatePayment() error = %v, want ErrOrderNotFound", err)
	}

	params := integrationPaymentParams(createdOrder.ID, "payment-key", "payment-request")
	created, err := paymentRepository.CreatePayment(ctx, customerID, params)
	if err != nil {
		t.Fatalf("CreatePayment() error = %v", err)
	}
	if created.Amount != 3000 || created.BalanceBefore != 5000 || created.BalanceAfter != 2000 ||
		created.Status != payment.StatusSucceeded || created.IdempotencyReplay {
		t.Fatalf("created payment = %+v", created)
	}
	assertAccountBalance(t, ctx, pool, customerID, 2000)
	assertOrderStatus(t, ctx, pool, createdOrder.ID, "paid")
	assertPaymentCount(t, ctx, pool, customerID, 1)
	var debitEntries int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		FROM payments AS p
		JOIN account_balance_entries AS e
		  ON e.account_id = p.account_id
		 AND e.balance_version = p.account_balance_version
		 AND e.balance_before = p.balance_before
		 AND e.balance_after = p.balance_after
		 AND e.debit_amount = p.amount
		WHERE p.id = $1
	`, created.ID).Scan(&debitEntries); err != nil {
		t.Fatalf("query payment debit entry: %v", err)
	}
	if debitEntries != 1 {
		t.Fatalf("payment debit entry count = %d, want 1", debitEntries)
	}
	if _, err := pool.Exec(ctx, `UPDATE payments SET status = status WHERE id = $1`, created.ID); err == nil {
		t.Fatal("immutable payment accepted an update")
	}

	replayParams := integrationPaymentParams(createdOrder.ID, "payment-key", "payment-request")
	replayed, err := paymentRepository.CreatePayment(ctx, customerID, replayParams)
	if err != nil {
		t.Fatalf("replayed CreatePayment() error = %v", err)
	}
	if !replayed.IdempotencyReplay || replayed.ID != created.ID || replayed.CreatedAt != created.CreatedAt {
		t.Fatalf("replayed payment = %+v, original = %+v", replayed, created)
	}
	assertAccountBalance(t, ctx, pool, customerID, 2000)
	assertPaymentCount(t, ctx, pool, customerID, 1)

	if _, err := paymentRepository.CreatePayment(
		ctx, customerID, integrationPaymentParams(createdOrder.ID, "payment-key", "different-request"),
	); !errors.Is(err, payment.ErrIdempotencyConflict) {
		t.Fatalf("same key different request error = %v, want ErrIdempotencyConflict", err)
	}
	if _, err := paymentRepository.CreatePayment(
		ctx, customerID, integrationPaymentParams(createdOrder.ID, "different-payment-key", "payment-request"),
	); !errors.Is(err, payment.ErrOrderAlreadyPaid) {
		t.Fatalf("different key paid order error = %v, want ErrOrderAlreadyPaid", err)
	}

	if _, err := paymentRepository.GetPayment(
		ctx, payment.Actor{UserID: customerID, Role: payment.RoleCustomer}, created.ID,
	); err != nil {
		t.Fatalf("customer GetPayment() error = %v", err)
	}
	if _, err := paymentRepository.GetPayment(
		ctx, payment.Actor{UserID: adminID, Role: payment.RoleAdmin}, created.ID,
	); err != nil {
		t.Fatalf("admin GetPayment() error = %v", err)
	}
	if _, err := paymentRepository.GetPayment(
		ctx, payment.Actor{UserID: otherCustomerID, Role: payment.RoleCustomer}, created.ID,
	); !errors.Is(err, payment.ErrPaymentNotFound) {
		t.Fatalf("cross-customer GetPayment() error = %v, want ErrPaymentNotFound", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET status = 'disabled', updated_at = now() WHERE id = $1`, customerID); err != nil {
		t.Fatalf("disable payment customer: %v", err)
	}
	if _, err := paymentRepository.GetPayment(
		ctx, payment.Actor{UserID: customerID, Role: payment.RoleCustomer}, created.ID,
	); !errors.Is(err, payment.ErrAccountDisabled) {
		t.Fatalf("disabled GetPayment() error = %v, want ErrAccountDisabled", err)
	}
}

func TestPaymentRepositoryInsufficientFundsRollsBackAndCanRetry(t *testing.T) {
	ctx, pool, orderRepository := openOrderRepositoryIntegration(t)
	paymentRepository := NewPaymentRepository(pool, 5*time.Second, 10*time.Second, 2*time.Second)
	adminID := createCommerceActor(t, ctx, pool, commerce.RoleAdmin)
	customerID := createCommerceActor(t, ctx, pool, commerce.RoleCustomer)
	productID := uuid.New()
	userIDs := []uuid.UUID{adminID, customerID}
	registerCommerceCleanup(t, pool, userIDs, []uuid.UUID{productID})
	registerPaymentCleanup(t, pool, userIDs)
	createIntegrationProduct(t, ctx, orderRepository, adminID, productID, uniqueIntegrationSKU("FUNDS"), 800, 1)
	setAccountBalance(t, ctx, pool, customerID, 100)
	createdOrder, err := orderRepository.CreateOrder(
		ctx, customerID, idempotentOrderParams(productID, 1, "funds-order", "funds-order"),
	)
	if err != nil {
		t.Fatalf("create insufficient-funds order: %v", err)
	}
	params := integrationPaymentParams(createdOrder.ID, "funds-payment", "funds-payment")
	if _, err := paymentRepository.CreatePayment(ctx, customerID, params); !errors.Is(err, payment.ErrInsufficientFunds) {
		t.Fatalf("CreatePayment() error = %v, want ErrInsufficientFunds", err)
	}
	assertAccountBalance(t, ctx, pool, customerID, 100)
	assertOrderStatus(t, ctx, pool, createdOrder.ID, "pending")
	assertPaymentCount(t, ctx, pool, customerID, 0)
	assertIdempotencyRows(t, ctx, pool, customerID, idempotency.PaymentCreateOperation, 0)

	setAccountBalance(t, ctx, pool, customerID, 800)
	if _, err := paymentRepository.CreatePayment(ctx, customerID, params); err != nil {
		t.Fatalf("retry after funding error = %v", err)
	}
	assertAccountBalance(t, ctx, pool, customerID, 0)
	assertOrderStatus(t, ctx, pool, createdOrder.ID, "paid")
	assertPaymentCount(t, ctx, pool, customerID, 1)
}

func TestPaymentRepositoryConcurrentSameKeyDebitsOnce(t *testing.T) {
	ctx, pool, orderRepository := openOrderRepositoryIntegration(t)
	paymentRepository := NewPaymentRepository(pool, 5*time.Second, 10*time.Second, 2*time.Second)
	adminID := createCommerceActor(t, ctx, pool, commerce.RoleAdmin)
	customerID := createCommerceActor(t, ctx, pool, commerce.RoleCustomer)
	productID := uuid.New()
	userIDs := []uuid.UUID{adminID, customerID}
	registerCommerceCleanup(t, pool, userIDs, []uuid.UUID{productID})
	registerPaymentCleanup(t, pool, userIDs)
	createIntegrationProduct(t, ctx, orderRepository, adminID, productID, uniqueIntegrationSKU("PAY-IDEM"), 600, 1)
	setAccountBalance(t, ctx, pool, customerID, 1000)
	createdOrder, err := orderRepository.CreateOrder(
		ctx, customerID, idempotentOrderParams(productID, 1, "pay-idem-order", "pay-idem-order"),
	)
	if err != nil {
		t.Fatalf("create idempotent payment order: %v", err)
	}

	const contenders = 20
	start := make(chan struct{})
	outcomes := make(chan payment.Payment, contenders)
	errorsCh := make(chan error, contenders)
	var waitGroup sync.WaitGroup
	for range contenders {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			<-start
			result, err := paymentRepository.CreatePayment(
				ctx, customerID, integrationPaymentParams(createdOrder.ID, "shared-payment-key", "shared-payment"),
			)
			outcomes <- result
			errorsCh <- err
		}()
	}
	close(start)
	waitGroup.Wait()
	close(outcomes)
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Fatalf("concurrent CreatePayment() error = %v", err)
		}
	}
	var paymentID uuid.UUID
	var originals, replays int
	for result := range outcomes {
		if paymentID == uuid.Nil {
			paymentID = result.ID
		}
		if result.ID != paymentID {
			t.Fatalf("concurrent idempotent payments differ: %s and %s", paymentID, result.ID)
		}
		if result.IdempotencyReplay {
			replays++
		} else {
			originals++
		}
	}
	if originals != 1 || replays != contenders-1 {
		t.Fatalf("payment outcomes = %d originals, %d replays", originals, replays)
	}
	assertAccountBalance(t, ctx, pool, customerID, 400)
	assertPaymentCount(t, ctx, pool, customerID, 1)
}

func TestPaymentRepositoryConcurrentOrdersCannotOverdraw(t *testing.T) {
	ctx, pool, orderRepository := openOrderRepositoryIntegration(t)
	paymentRepository := NewPaymentRepository(pool, 5*time.Second, 10*time.Second, 2*time.Second)
	adminID := createCommerceActor(t, ctx, pool, commerce.RoleAdmin)
	customerID := createCommerceActor(t, ctx, pool, commerce.RoleCustomer)
	productID := uuid.New()
	userIDs := []uuid.UUID{adminID, customerID}
	registerCommerceCleanup(t, pool, userIDs, []uuid.UUID{productID})
	registerPaymentCleanup(t, pool, userIDs)
	createIntegrationProduct(t, ctx, orderRepository, adminID, productID, uniqueIntegrationSKU("OVERDRAW"), 700, 2)
	setAccountBalance(t, ctx, pool, customerID, 1000)
	firstOrder, err := orderRepository.CreateOrder(
		ctx, customerID, idempotentOrderParams(productID, 1, "overdraw-order-1", "overdraw-order-1"),
	)
	if err != nil {
		t.Fatalf("create first overdraw order: %v", err)
	}
	secondOrder, err := orderRepository.CreateOrder(
		ctx, customerID, idempotentOrderParams(productID, 1, "overdraw-order-2", "overdraw-order-2"),
	)
	if err != nil {
		t.Fatalf("create second overdraw order: %v", err)
	}
	orders := []uuid.UUID{firstOrder.ID, secondOrder.ID}
	start := make(chan struct{})
	errorsCh := make(chan error, 2)
	var waitGroup sync.WaitGroup
	for index, orderID := range orders {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			<-start
			_, err := paymentRepository.CreatePayment(
				ctx, customerID,
				integrationPaymentParams(orderID, "overdraw-payment-"+string(rune('a'+index)), orderID.String()),
			)
			errorsCh <- err
		}()
	}
	close(start)
	waitGroup.Wait()
	close(errorsCh)
	var successes, insufficient int
	for err := range errorsCh {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, payment.ErrInsufficientFunds):
			insufficient++
		default:
			t.Fatalf("concurrent overdraw payment error = %v", err)
		}
	}
	if successes != 1 || insufficient != 1 {
		t.Fatalf("overdraw outcomes = %d successes, %d insufficient", successes, insufficient)
	}
	assertAccountBalance(t, ctx, pool, customerID, 300)
	assertPaymentCount(t, ctx, pool, customerID, 1)
	var paid, pending int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE status = 'paid'), count(*) FILTER (WHERE status = 'pending')
		FROM orders WHERE id = ANY($1::uuid[])
	`, orders).Scan(&paid, &pending); err != nil {
		t.Fatalf("count overdraw order statuses: %v", err)
	}
	if paid != 1 || pending != 1 {
		t.Fatalf("overdraw order statuses = %d paid, %d pending", paid, pending)
	}
}

func TestPaymentRepositoryConcurrentDifferentKeysSameOrderDebitsOnce(t *testing.T) {
	ctx, pool, orderRepository := openOrderRepositoryIntegration(t)
	paymentRepository := NewPaymentRepository(pool, 5*time.Second, 10*time.Second, 2*time.Second)
	adminID := createCommerceActor(t, ctx, pool, commerce.RoleAdmin)
	customerID := createCommerceActor(t, ctx, pool, commerce.RoleCustomer)
	productID := uuid.New()
	userIDs := []uuid.UUID{adminID, customerID}
	registerCommerceCleanup(t, pool, userIDs, []uuid.UUID{productID})
	registerPaymentCleanup(t, pool, userIDs)
	createIntegrationProduct(t, ctx, orderRepository, adminID, productID, uniqueIntegrationSKU("PAY-RACE"), 600, 1)
	setAccountBalance(t, ctx, pool, customerID, 2000)
	createdOrder, err := orderRepository.CreateOrder(
		ctx, customerID, idempotentOrderParams(productID, 1, "pay-race-order", "pay-race-order"),
	)
	if err != nil {
		t.Fatalf("create payment race order: %v", err)
	}
	start := make(chan struct{})
	errorsCh := make(chan error, 2)
	var waitGroup sync.WaitGroup
	for _, key := range []string{"different-key-a", "different-key-b"} {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			<-start
			_, err := paymentRepository.CreatePayment(
				ctx, customerID, integrationPaymentParams(createdOrder.ID, key, createdOrder.ID.String()),
			)
			errorsCh <- err
		}()
	}
	close(start)
	waitGroup.Wait()
	close(errorsCh)
	var success, alreadyPaid int
	for err := range errorsCh {
		switch {
		case err == nil:
			success++
		case errors.Is(err, payment.ErrOrderAlreadyPaid):
			alreadyPaid++
		default:
			t.Fatalf("same-order payment race error = %v", err)
		}
	}
	if success != 1 || alreadyPaid != 1 {
		t.Fatalf("same-order outcomes = %d success, %d already paid", success, alreadyPaid)
	}
	assertAccountBalance(t, ctx, pool, customerID, 1400)
	assertPaymentCount(t, ctx, pool, customerID, 1)
}

func TestPaymentRepositoryLateInsertFailureRollsBackDebit(t *testing.T) {
	ctx, pool, orderRepository := openOrderRepositoryIntegration(t)
	paymentRepository := NewPaymentRepository(pool, 5*time.Second, 10*time.Second, 2*time.Second)
	adminID := createCommerceActor(t, ctx, pool, commerce.RoleAdmin)
	customerID := createCommerceActor(t, ctx, pool, commerce.RoleCustomer)
	productID := uuid.New()
	userIDs := []uuid.UUID{adminID, customerID}
	registerCommerceCleanup(t, pool, userIDs, []uuid.UUID{productID})
	registerPaymentCleanup(t, pool, userIDs)
	createIntegrationProduct(t, ctx, orderRepository, adminID, productID, uniqueIntegrationSKU("PAY-LATE"), 500, 2)
	setAccountBalance(t, ctx, pool, customerID, 2000)
	firstOrder, err := orderRepository.CreateOrder(
		ctx, customerID, idempotentOrderParams(productID, 1, "pay-late-order-1", "pay-late-order-1"),
	)
	if err != nil {
		t.Fatalf("create first late-failure order: %v", err)
	}
	secondOrder, err := orderRepository.CreateOrder(
		ctx, customerID, idempotentOrderParams(productID, 1, "pay-late-order-2", "pay-late-order-2"),
	)
	if err != nil {
		t.Fatalf("create second late-failure order: %v", err)
	}
	firstPayment, err := paymentRepository.CreatePayment(
		ctx, customerID, integrationPaymentParams(firstOrder.ID, "pay-late-first", "pay-late-first"),
	)
	if err != nil {
		t.Fatalf("create first late-failure payment: %v", err)
	}
	conflicting := integrationPaymentParams(secondOrder.ID, "pay-late-second", "pay-late-second")
	conflicting.PaymentID = firstPayment.ID
	if _, err := paymentRepository.CreatePayment(ctx, customerID, conflicting); err == nil {
		t.Fatal("late-failure CreatePayment() succeeded with a duplicate payment ID")
	}
	assertAccountBalance(t, ctx, pool, customerID, 1500)
	assertOrderStatus(t, ctx, pool, firstOrder.ID, "paid")
	assertOrderStatus(t, ctx, pool, secondOrder.ID, "pending")
	assertPaymentCount(t, ctx, pool, customerID, 1)
	assertIdempotencyRows(t, ctx, pool, customerID, idempotency.PaymentCreateOperation, 1)
}

func TestPaymentSchemaRejectsPaymentWithoutDebitEntry(t *testing.T) {
	ctx, pool, orderRepository := openOrderRepositoryIntegration(t)
	adminID := createCommerceActor(t, ctx, pool, commerce.RoleAdmin)
	customerID := createCommerceActor(t, ctx, pool, commerce.RoleCustomer)
	productID := uuid.New()
	userIDs := []uuid.UUID{adminID, customerID}
	registerCommerceCleanup(t, pool, userIDs, []uuid.UUID{productID})
	registerPaymentCleanup(t, pool, userIDs)
	createIntegrationProduct(t, ctx, orderRepository, adminID, productID, uniqueIntegrationSKU("PAY-CONSTRAINT"), 500, 1)
	setAccountBalance(t, ctx, pool, customerID, 1000)
	createdOrder, err := orderRepository.CreateOrder(
		ctx, customerID, idempotentOrderParams(productID, 1, "constraint-order", "constraint-order"),
	)
	if err != nil {
		t.Fatalf("create payment constraint order: %v", err)
	}
	var accountID uuid.UUID
	var balanceVersion int64
	if err := pool.QueryRow(ctx, `
		SELECT id, balance_version FROM accounts WHERE user_id = $1
	`, customerID).Scan(&accountID, &balanceVersion); err != nil {
		t.Fatalf("query constraint account: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE orders SET status = 'paid', updated_at = now() WHERE id = $1`, createdOrder.ID); err != nil {
		t.Fatalf("mark constraint order paid: %v", err)
	}
	_, err = pool.Exec(ctx, `
		INSERT INTO payments (
			id, order_id, user_id, account_id, account_balance_version, status, currency, amount,
			balance_before, balance_after, created_at
		) VALUES ($1, $2, $3, $4, $5, 'succeeded', 'USD', 500, 1500, 1000, now())
	`, uuid.New(), createdOrder.ID, customerID, accountID, balanceVersion)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
		t.Fatalf("payment without debit entry error = %v, want SQLSTATE 23503", err)
	}
	assertPaymentCount(t, ctx, pool, customerID, 0)
	assertAccountBalance(t, ctx, pool, customerID, 1000)
	assertOrderStatus(t, ctx, pool, createdOrder.ID, "paid")
	if _, err := pool.Exec(ctx, `UPDATE orders SET status = 'pending', updated_at = now() WHERE id = $1`, createdOrder.ID); err == nil {
		t.Fatal("paid order accepted a status reversal")
	}
}

func TestPaymentRepositoryCommitResolution(t *testing.T) {
	ctx, pool, orderRepository := openOrderRepositoryIntegration(t)
	paymentRepository := NewPaymentRepository(pool, 5*time.Second, 10*time.Second, 2*time.Second)
	adminID := createCommerceActor(t, ctx, pool, commerce.RoleAdmin)
	customerID := createCommerceActor(t, ctx, pool, commerce.RoleCustomer)
	productID := uuid.New()
	userIDs := []uuid.UUID{adminID, customerID}
	registerCommerceCleanup(t, pool, userIDs, []uuid.UUID{productID})
	registerPaymentCleanup(t, pool, userIDs)
	createIntegrationProduct(t, ctx, orderRepository, adminID, productID, uniqueIntegrationSKU("PAY-COMMIT"), 500, 1)
	setAccountBalance(t, ctx, pool, customerID, 1000)
	createdOrder, err := orderRepository.CreateOrder(
		ctx, customerID, idempotentOrderParams(productID, 1, "pay-commit-order", "pay-commit-order"),
	)
	if err != nil {
		t.Fatalf("create payment commit order: %v", err)
	}
	paymentRepository.commitTransaction = func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		return &pgconn.PgError{Code: "40003", Severity: "ERROR", Message: "synthetic statement completion unknown"}
	}
	result, err := paymentRepository.CreatePayment(
		ctx, customerID, integrationPaymentParams(createdOrder.ID, "pay-commit", "pay-commit"),
	)
	if err != nil {
		t.Fatalf("resolved CreatePayment() error = %v", err)
	}
	if !result.IdempotencyReplay {
		t.Fatal("resolved payment commit was not marked as replay")
	}
	assertAccountBalance(t, ctx, pool, customerID, 500)
	assertOrderStatus(t, ctx, pool, createdOrder.ID, "paid")
	assertPaymentCount(t, ctx, pool, customerID, 1)
}

func TestPaymentRepositoryUnknownCommitOutcomeDoesNotRetry(t *testing.T) {
	ctx, pool, orderRepository := openOrderRepositoryIntegration(t)
	paymentRepository := NewPaymentRepository(pool, 5*time.Second, 10*time.Second, 75*time.Millisecond)
	adminID := createCommerceActor(t, ctx, pool, commerce.RoleAdmin)
	customerID := createCommerceActor(t, ctx, pool, commerce.RoleCustomer)
	productID := uuid.New()
	userIDs := []uuid.UUID{adminID, customerID}
	registerCommerceCleanup(t, pool, userIDs, []uuid.UUID{productID})
	registerPaymentCleanup(t, pool, userIDs)
	createIntegrationProduct(t, ctx, orderRepository, adminID, productID, uniqueIntegrationSKU("PAY-UNKNOWN"), 500, 1)
	setAccountBalance(t, ctx, pool, customerID, 1000)
	createdOrder, err := orderRepository.CreateOrder(
		ctx, customerID, idempotentOrderParams(productID, 1, "pay-unknown-order", "pay-unknown-order"),
	)
	if err != nil {
		t.Fatalf("create unknown payment order: %v", err)
	}
	paymentRepository.commitTransaction = func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.Rollback(ctx); err != nil {
			return err
		}
		return errors.New("synthetic unknown payment commit outcome")
	}
	_, err = paymentRepository.CreatePayment(
		ctx, customerID, integrationPaymentParams(createdOrder.ID, "pay-unknown", "pay-unknown"),
	)
	if !errors.Is(err, payment.ErrOperationOutcomeUnknown) {
		t.Fatalf("CreatePayment() error = %v, want ErrOperationOutcomeUnknown", err)
	}
	assertAccountBalance(t, ctx, pool, customerID, 1000)
	assertOrderStatus(t, ctx, pool, createdOrder.ID, "pending")
	assertPaymentCount(t, ctx, pool, customerID, 0)
	assertIdempotencyRows(t, ctx, pool, customerID, idempotency.PaymentCreateOperation, 0)
}

func integrationPaymentParams(orderID uuid.UUID, key, semanticRequest string) payment.CreateParams {
	keyHash := idempotency.KeyHash(key)
	requestHash := idempotency.RequestHash(idempotency.PaymentCreateOperation, []byte(semanticRequest))
	return payment.CreateParams{
		PaymentID: uuid.New(), OrderID: orderID, KeyHash: keyHash[:], RequestHash: requestHash[:],
	}
}

func setAccountBalance(t *testing.T, ctx context.Context, pool *pgxpool.Pool, userID uuid.UUID, balance int64) {
	t.Helper()
	if _, err := pool.Exec(ctx, `UPDATE accounts SET balance = $2, updated_at = now() WHERE user_id = $1`, userID, balance); err != nil {
		t.Fatalf("set account balance: %v", err)
	}
}

func assertAccountBalance(t *testing.T, ctx context.Context, pool *pgxpool.Pool, userID uuid.UUID, want int64) {
	t.Helper()
	var balance int64
	if err := pool.QueryRow(ctx, `SELECT balance FROM accounts WHERE user_id = $1`, userID).Scan(&balance); err != nil {
		t.Fatalf("query account balance: %v", err)
	}
	if balance != want {
		t.Fatalf("account balance = %d, want %d", balance, want)
	}
}

func assertOrderStatus(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orderID uuid.UUID, want string) {
	t.Helper()
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM orders WHERE id = $1`, orderID).Scan(&status); err != nil {
		t.Fatalf("query order status: %v", err)
	}
	if status != want {
		t.Fatalf("order status = %q, want %q", status, want)
	}
}

func assertPaymentCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, userID uuid.UUID, want int) {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM payments WHERE user_id = $1`, userID).Scan(&count); err != nil {
		t.Fatalf("count payments: %v", err)
	}
	if count != want {
		t.Fatalf("payment count = %d, want %d", count, want)
	}
}

func registerPaymentCleanup(t *testing.T, pool *pgxpool.Pool, userIDs []uuid.UUID) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := pool.Exec(ctx, `DELETE FROM payments WHERE user_id = ANY($1::uuid[])`, userIDs); err != nil {
			t.Errorf("delete integration payments: %v", err)
		}
	})
}
