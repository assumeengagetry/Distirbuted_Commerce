//go:build integration

package database

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/assumeengagetry/distributed-commerce/internal/idempotency"
	commerce "github.com/assumeengagetry/distributed-commerce/internal/order"
)

func TestOrderRepositoryProductAndInventoryTransactions(t *testing.T) {
	ctx, pool, repository := openOrderRepositoryIntegration(t)
	adminID := createCommerceActor(t, ctx, pool, commerce.RoleAdmin)
	productID := uuid.New()
	sku := uniqueIntegrationSKU("PHASE3")
	registerCommerceCleanup(t, pool, []uuid.UUID{adminID}, []uuid.UUID{productID})

	created, err := repository.CreateProduct(ctx, adminID, commerce.CreateProductParams{
		ProductID: productID,
		Request: commerce.CreateProductRequest{
			SKU: sku, Name: "Phase 3 Product", Description: "inventory test",
			PriceAmount: 1299, Currency: commerce.CurrencyUSD,
			Status: commerce.ProductStatusInactive, InitialQuantity: 3,
		},
	})
	if err != nil {
		t.Fatalf("CreateProduct() error = %v", err)
	}
	if created.Product.Version != 1 || created.Inventory.Version != 1 || created.Inventory.Quantity != 3 {
		t.Fatalf("created product = %+v", created)
	}
	if _, err := repository.GetProduct(ctx, productID); !errors.Is(err, commerce.ErrProductNotFound) {
		t.Fatalf("GetProduct(inactive) error = %v, want ErrProductNotFound", err)
	}

	duplicateID := uuid.New()
	if _, err := repository.CreateProduct(ctx, adminID, commerce.CreateProductParams{
		ProductID: duplicateID,
		Request: commerce.CreateProductRequest{
			SKU: sku, Name: "Duplicate", PriceAmount: 100,
			Currency: commerce.CurrencyUSD, Status: commerce.ProductStatusActive,
		},
	}); !errors.Is(err, commerce.ErrSKUAlreadyExists) {
		t.Fatalf("duplicate CreateProduct() error = %v, want ErrSKUAlreadyExists", err)
	}
	var duplicateRows int
	if err := pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM products WHERE id = $1)
		     + (SELECT count(*) FROM inventories WHERE product_id = $1)
	`, duplicateID).Scan(&duplicateRows); err != nil {
		t.Fatalf("count duplicate product rows: %v", err)
	}
	if duplicateRows != 0 {
		t.Fatalf("duplicate product transaction left %d rows", duplicateRows)
	}

	active := commerce.ProductStatusActive
	updatedName := "Available Product"
	updated, err := repository.UpdateProduct(ctx, adminID, productID, commerce.UpdateProductRequest{
		ExpectedVersion: 1, Name: &updatedName, Status: &active,
	})
	if err != nil {
		t.Fatalf("UpdateProduct() error = %v", err)
	}
	if updated.Product.Version != 2 || updated.Product.Status != active {
		t.Fatalf("updated product = %+v", updated.Product)
	}
	if _, err := repository.UpdateProduct(ctx, adminID, productID, commerce.UpdateProductRequest{
		ExpectedVersion: 1, Name: &updatedName,
	}); !errors.Is(err, commerce.ErrProductVersionConflict) {
		t.Fatalf("stale UpdateProduct() error = %v, want ErrProductVersionConflict", err)
	}

	inventory, err := repository.AdjustInventory(ctx, adminID, productID, commerce.AdjustInventoryRequest{
		ExpectedVersion: 1, Delta: -2,
	})
	if err != nil {
		t.Fatalf("AdjustInventory() error = %v", err)
	}
	if inventory.Quantity != 1 || inventory.Version != 2 {
		t.Fatalf("adjusted inventory = %+v", inventory)
	}
	if _, err := repository.AdjustInventory(ctx, adminID, productID, commerce.AdjustInventoryRequest{
		ExpectedVersion: 2, Delta: -2,
	}); !errors.Is(err, commerce.ErrInventoryOutOfRange) {
		t.Fatalf("underflow AdjustInventory() error = %v, want ErrInventoryOutOfRange", err)
	}
	current, err := repository.GetAdminProduct(ctx, adminID, productID)
	if err != nil {
		t.Fatalf("GetAdminProduct() error = %v", err)
	}
	if current.Inventory.Quantity != 1 || current.Inventory.Version != 2 {
		t.Fatalf("inventory changed after rejected adjustment: %+v", current.Inventory)
	}
}

func TestOrderRepositoryAtomicRollbackAndSnapshots(t *testing.T) {
	ctx, pool, repository := openOrderRepositoryIntegration(t)
	adminID := createCommerceActor(t, ctx, pool, commerce.RoleAdmin)
	customerID := createCommerceActor(t, ctx, pool, commerce.RoleCustomer)
	otherCustomerID := createCommerceActor(t, ctx, pool, commerce.RoleCustomer)
	productA, productB := uuid.New(), uuid.New()
	if bytes.Compare(productA[:], productB[:]) > 0 {
		productA, productB = productB, productA
	}
	registerCommerceCleanup(
		t, pool, []uuid.UUID{adminID, customerID, otherCustomerID}, []uuid.UUID{productA, productB},
	)
	createIntegrationProduct(t, ctx, repository, adminID, productA, uniqueIntegrationSKU("ORDER-A"), 500, 2)
	createIntegrationProduct(t, ctx, repository, adminID, productB, uniqueIntegrationSKU("ORDER-B"), 700, 0)

	failedParams := integrationOrderParams(productA, productB, 2, 1)
	if _, err := repository.CreateOrder(ctx, customerID, failedParams); !errors.Is(err, commerce.ErrInsufficientInventory) {
		t.Fatalf("CreateOrder(insufficient) error = %v, want ErrInsufficientInventory", err)
	}
	assertCommerceCounts(t, ctx, pool, customerID, 0, 0)
	assertInventoryQuantity(t, ctx, pool, productA, 2)
	assertInventoryQuantity(t, ctx, pool, productB, 0)
	assertInventoryVersion(t, ctx, pool, productA, 1)
	assertInventoryVersion(t, ctx, pool, productB, 1)

	if _, err := repository.AdjustInventory(ctx, adminID, productB, commerce.AdjustInventoryRequest{
		ExpectedVersion: 1, Delta: 1,
	}); err != nil {
		t.Fatalf("restock product B: %v", err)
	}
	created, err := repository.CreateOrder(ctx, customerID, integrationOrderParams(productA, productB, 2, 1))
	if err != nil {
		t.Fatalf("CreateOrder() error = %v", err)
	}
	if created.TotalAmount != 1700 || created.Status != commerce.OrderStatusPending || len(created.Items) != 2 {
		t.Fatalf("created order = %+v", created)
	}
	assertCommerceCounts(t, ctx, pool, customerID, 1, 2)
	assertInventoryQuantity(t, ctx, pool, productA, 0)
	assertInventoryQuantity(t, ctx, pool, productB, 0)

	newPrice := int64(999)
	if _, err := repository.UpdateProduct(ctx, adminID, productA, commerce.UpdateProductRequest{
		ExpectedVersion: 1, PriceAmount: &newPrice,
	}); err != nil {
		t.Fatalf("update product price: %v", err)
	}
	history, err := repository.GetOrder(ctx, commerce.Actor{UserID: customerID, Role: commerce.RoleCustomer}, created.ID)
	if err != nil {
		t.Fatalf("GetOrder() error = %v", err)
	}
	for _, item := range history.Items {
		if item.ProductID == productA && (item.UnitPriceAmount != 500 || item.ProductVersion != 1) {
			t.Fatalf("product snapshot changed = %+v", item)
		}
	}
	if _, err := repository.GetOrder(
		ctx, commerce.Actor{UserID: otherCustomerID, Role: commerce.RoleCustomer}, created.ID,
	); !errors.Is(err, commerce.ErrOrderNotFound) {
		t.Fatalf("cross-customer GetOrder() error = %v, want ErrOrderNotFound", err)
	}
	adminHistory, err := repository.ListOrders(
		ctx, commerce.Actor{UserID: adminID, Role: commerce.RoleAdmin}, commerce.PageRequest{Limit: 20},
	)
	if err != nil || len(adminHistory.Orders) < 1 {
		t.Fatalf("admin ListOrders() = (%+v, %v)", adminHistory, err)
	}
}

func TestOrderRepositoryConcurrentSingleInventory(t *testing.T) {
	ctx, pool, repository := openOrderRepositoryIntegration(t)
	adminID := createCommerceActor(t, ctx, pool, commerce.RoleAdmin)
	customerID := createCommerceActor(t, ctx, pool, commerce.RoleCustomer)
	productID := uuid.New()
	registerCommerceCleanup(t, pool, []uuid.UUID{adminID, customerID}, []uuid.UUID{productID})
	createIntegrationProduct(t, ctx, repository, adminID, productID, uniqueIntegrationSKU("ONE-STOCK"), 2500, 1)

	const contenders = 20
	start := make(chan struct{})
	outcomes := make(chan error, contenders)
	var waitGroup sync.WaitGroup
	for range contenders {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			<-start
			params := repositoryOrderParams(commerce.CreateOrderParams{
				OrderID: uuid.New(),
				Items: []commerce.RequestedItem{{
					ItemID: uuid.New(), ProductID: productID, Quantity: 1, ExpectedProductVersion: 1,
				}},
			})
			_, err := repository.CreateOrder(ctx, customerID, params)
			outcomes <- err
		}()
	}
	close(start)
	waitGroup.Wait()
	close(outcomes)

	var successes, insufficient int
	for err := range outcomes {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, commerce.ErrInsufficientInventory):
			insufficient++
		default:
			t.Fatalf("concurrent CreateOrder() unexpected error = %v", err)
		}
	}
	if successes != 1 || insufficient != contenders-1 {
		t.Fatalf("concurrent outcomes = %d successes, %d insufficient", successes, insufficient)
	}
	assertInventoryQuantity(t, ctx, pool, productID, 0)
	assertCommerceCounts(t, ctx, pool, customerID, 1, 1)
}

func TestOrderRepositoryConcurrentIdempotentReplay(t *testing.T) {
	ctx, pool, repository := openOrderRepositoryIntegration(t)
	adminID := createCommerceActor(t, ctx, pool, commerce.RoleAdmin)
	customerID := createCommerceActor(t, ctx, pool, commerce.RoleCustomer)
	productID := uuid.New()
	registerCommerceCleanup(t, pool, []uuid.UUID{adminID, customerID}, []uuid.UUID{productID})
	createIntegrationProduct(t, ctx, repository, adminID, productID, uniqueIntegrationSKU("IDEM-ORDER"), 900, 1)

	const contenders = 20
	start := make(chan struct{})
	outcomes := make(chan commerce.Order, contenders)
	errorsCh := make(chan error, contenders)
	var waitGroup sync.WaitGroup
	for range contenders {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			<-start
			result, err := repository.CreateOrder(
				ctx, customerID, idempotentOrderParams(productID, 1, "shared-order-key", "shared-request"),
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
			t.Fatalf("idempotent CreateOrder() error = %v", err)
		}
	}
	var orderID uuid.UUID
	var original, replays int
	for result := range outcomes {
		if orderID == uuid.Nil {
			orderID = result.ID
		}
		if result.ID != orderID {
			t.Fatalf("idempotent results used different orders: %s and %s", orderID, result.ID)
		}
		if result.IdempotencyReplay {
			replays++
		} else {
			original++
		}
	}
	if original != 1 || replays != contenders-1 {
		t.Fatalf("idempotent outcomes = %d original, %d replays", original, replays)
	}
	assertInventoryQuantity(t, ctx, pool, productID, 0)
	assertCommerceCounts(t, ctx, pool, customerID, 1, 1)
	var storedKeyHash []byte
	if err := pool.QueryRow(ctx, `
		SELECT key_hash FROM idempotency_keys
		WHERE actor_id = $1 AND operation = $2
	`, customerID, idempotency.OrderCreateOperation).Scan(&storedKeyHash); err != nil {
		t.Fatalf("query stored idempotency key hash: %v", err)
	}
	if len(storedKeyHash) != 32 || bytes.Equal(storedKeyHash, []byte("shared-order-key")) {
		t.Fatalf("stored key is not a SHA-256 digest: %x", storedKeyHash)
	}

	_, err := repository.CreateOrder(
		ctx, customerID, idempotentOrderParams(productID, 1, "shared-order-key", "different-request"),
	)
	if !errors.Is(err, commerce.ErrIdempotencyConflict) {
		t.Fatalf("same key different request error = %v, want ErrIdempotencyConflict", err)
	}
}

func TestOrderRepositoryIdempotencyFailureAndCommitResolution(t *testing.T) {
	ctx, pool, repository := openOrderRepositoryIntegration(t)
	adminID := createCommerceActor(t, ctx, pool, commerce.RoleAdmin)
	customerID := createCommerceActor(t, ctx, pool, commerce.RoleCustomer)
	productID, secondProductID := uuid.New(), uuid.New()
	registerCommerceCleanup(
		t, pool, []uuid.UUID{adminID, customerID}, []uuid.UUID{productID, secondProductID},
	)
	createIntegrationProduct(t, ctx, repository, adminID, productID, uniqueIntegrationSKU("COMMIT-ORDER"), 1200, 1)

	failed := idempotentOrderParams(productID, 2, "retry-after-stock", "quantity-two")
	if _, err := repository.CreateOrder(ctx, customerID, failed); !errors.Is(err, commerce.ErrInsufficientInventory) {
		t.Fatalf("insufficient CreateOrder() error = %v", err)
	}
	assertIdempotencyRows(t, ctx, pool, customerID, "orders.create.v1", 0)
	if _, err := repository.AdjustInventory(ctx, adminID, productID, commerce.AdjustInventoryRequest{
		ExpectedVersion: 1, Delta: 1,
	}); err != nil {
		t.Fatalf("restock after failed idempotent order: %v", err)
	}
	if _, err := repository.CreateOrder(ctx, customerID, failed); err != nil {
		t.Fatalf("retry after restock error = %v", err)
	}
	assertIdempotencyRows(t, ctx, pool, customerID, "orders.create.v1", 1)

	if _, err := repository.CreateProduct(ctx, adminID, commerce.CreateProductParams{
		ProductID: secondProductID,
		Request: commerce.CreateProductRequest{
			SKU: uniqueIntegrationSKU("ACK-ORDER"), Name: "Commit Ack Product", PriceAmount: 500,
			Currency: commerce.CurrencyUSD, Status: commerce.ProductStatusActive, InitialQuantity: 1,
		},
	}); err != nil {
		t.Fatalf("create commit resolution product: %v", err)
	}
	repository.commitTransaction = func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		return &pgconn.PgError{Code: "08007", Severity: "ERROR", Message: "synthetic transaction resolution unknown"}
	}
	resolved, err := repository.CreateOrder(
		ctx, customerID, idempotentOrderParams(secondProductID, 1, "commit-resolve", "commit-resolve"),
	)
	if err != nil {
		t.Fatalf("resolved CreateOrder() error = %v", err)
	}
	if !resolved.IdempotencyReplay {
		t.Fatal("resolved commit was not marked as an idempotency replay")
	}
	assertInventoryQuantity(t, ctx, pool, secondProductID, 0)
}

func TestOrderRepositoryUnknownCommitOutcomeDoesNotRetry(t *testing.T) {
	ctx, pool, repository := openOrderRepositoryIntegration(t)
	adminID := createCommerceActor(t, ctx, pool, commerce.RoleAdmin)
	customerID := createCommerceActor(t, ctx, pool, commerce.RoleCustomer)
	productID := uuid.New()
	registerCommerceCleanup(t, pool, []uuid.UUID{adminID, customerID}, []uuid.UUID{productID})
	createIntegrationProduct(t, ctx, repository, adminID, productID, uniqueIntegrationSKU("UNKNOWN-ORDER"), 700, 1)
	repository.commitResolutionTimeout = 75 * time.Millisecond
	repository.commitTransaction = func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.Rollback(ctx); err != nil {
			return err
		}
		return errors.New("synthetic unknown commit outcome")
	}
	_, err := repository.CreateOrder(
		ctx, customerID, idempotentOrderParams(productID, 1, "unknown-outcome", "unknown-outcome"),
	)
	if !errors.Is(err, commerce.ErrOperationOutcomeUnknown) {
		t.Fatalf("CreateOrder() error = %v, want ErrOperationOutcomeUnknown", err)
	}
	assertInventoryQuantity(t, ctx, pool, productID, 1)
	assertCommerceCounts(t, ctx, pool, customerID, 0, 0)
	assertIdempotencyRows(t, ctx, pool, customerID, "orders.create.v1", 0)
}

func TestOrderRepositoryConcurrentOpposingItemOrder(t *testing.T) {
	ctx, pool, repository := openOrderRepositoryIntegration(t)
	adminID := createCommerceActor(t, ctx, pool, commerce.RoleAdmin)
	customerID := createCommerceActor(t, ctx, pool, commerce.RoleCustomer)
	productA, productB := uuid.New(), uuid.New()
	registerCommerceCleanup(t, pool, []uuid.UUID{adminID, customerID}, []uuid.UUID{productA, productB})
	createIntegrationProduct(t, ctx, repository, adminID, productA, uniqueIntegrationSKU("LOCK-A"), 100, 2)
	createIntegrationProduct(t, ctx, repository, adminID, productB, uniqueIntegrationSKU("LOCK-B"), 200, 2)
	requests := []commerce.CreateOrderParams{
		repositoryOrderParams(commerce.CreateOrderParams{OrderID: uuid.New(), Items: []commerce.RequestedItem{
			{ItemID: uuid.New(), ProductID: productA, Quantity: 1, ExpectedProductVersion: 1},
			{ItemID: uuid.New(), ProductID: productB, Quantity: 1, ExpectedProductVersion: 1},
		}}),
		repositoryOrderParams(commerce.CreateOrderParams{OrderID: uuid.New(), Items: []commerce.RequestedItem{
			{ItemID: uuid.New(), ProductID: productB, Quantity: 1, ExpectedProductVersion: 1},
			{ItemID: uuid.New(), ProductID: productA, Quantity: 1, ExpectedProductVersion: 1},
		}}),
	}
	start := make(chan struct{})
	outcomes := make(chan error, len(requests))
	var waitGroup sync.WaitGroup
	for _, request := range requests {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			<-start
			_, err := repository.CreateOrder(ctx, customerID, request)
			outcomes <- err
		}()
	}
	close(start)
	waitGroup.Wait()
	close(outcomes)
	for err := range outcomes {
		if err != nil {
			t.Fatalf("opposing CreateOrder() error = %v", err)
		}
	}
	assertInventoryQuantity(t, ctx, pool, productA, 0)
	assertInventoryQuantity(t, ctx, pool, productB, 0)
	assertCommerceCounts(t, ctx, pool, customerID, 2, 4)
}

func TestOrderRepositoryKeysetPaginationWithEqualTimestamps(t *testing.T) {
	ctx, pool, repository := openOrderRepositoryIntegration(t)
	adminID := createCommerceActor(t, ctx, pool, commerce.RoleAdmin)
	customerID := createCommerceActor(t, ctx, pool, commerce.RoleCustomer)
	productIDs := []uuid.UUID{uuid.New(), uuid.New(), uuid.New()}
	registerCommerceCleanup(t, pool, []uuid.UUID{adminID, customerID}, productIDs)
	for index, productID := range productIDs {
		createIntegrationProduct(
			t, ctx, repository, adminID, productID,
			uniqueIntegrationSKU(fmt.Sprintf("PAGE-%d", index)), int64(100+index), 1,
		)
		params := repositoryOrderParams(commerce.CreateOrderParams{
			OrderID: uuid.New(),
			Items: []commerce.RequestedItem{{
				ItemID: uuid.New(), ProductID: productID, Quantity: 1, ExpectedProductVersion: 1,
			}},
		})
		_, err := repository.CreateOrder(ctx, customerID, params)
		if err != nil {
			t.Fatalf("create pagination order %d: %v", index, err)
		}
	}
	fixedTime := time.Date(2099, time.January, 1, 0, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(ctx, `
		UPDATE products SET created_at = $2, updated_at = $2 WHERE id = ANY($1::uuid[])
	`, productIDs, fixedTime); err != nil {
		t.Fatalf("align product timestamps: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE orders SET created_at = $2, updated_at = $2 WHERE user_id = $1
	`, customerID, fixedTime); err != nil {
		t.Fatalf("align order timestamps: %v", err)
	}

	firstProducts, err := repository.ListProducts(ctx, commerce.PageRequest{Limit: 2})
	if err != nil || len(firstProducts.Products) != 2 || firstProducts.NextCursor == nil {
		t.Fatalf("first product page = (%+v, %v)", firstProducts, err)
	}
	secondProducts, err := repository.ListProducts(ctx, commerce.PageRequest{Limit: 2, Cursor: firstProducts.NextCursor})
	if err != nil || len(secondProducts.Products) != 1 || secondProducts.NextCursor != nil {
		t.Fatalf("second product page = (%+v, %v)", secondProducts, err)
	}
	assertUniqueProductPages(t, firstProducts.Products, secondProducts.Products, productIDs)

	firstInventory, err := repository.ListInventory(ctx, adminID, commerce.PageRequest{Limit: 2})
	if err != nil || len(firstInventory.Inventories) != 2 || firstInventory.NextCursor == nil {
		t.Fatalf("first inventory page = (%+v, %v)", firstInventory, err)
	}
	secondInventory, err := repository.ListInventory(ctx, adminID, commerce.PageRequest{
		Limit: 2, Cursor: firstInventory.NextCursor,
	})
	if err != nil || len(secondInventory.Inventories) != 1 || secondInventory.NextCursor != nil {
		t.Fatalf("second inventory page = (%+v, %v)", secondInventory, err)
	}

	actor := commerce.Actor{UserID: customerID, Role: commerce.RoleCustomer}
	firstOrders, err := repository.ListOrders(ctx, actor, commerce.PageRequest{Limit: 2})
	if err != nil || len(firstOrders.Orders) != 2 || firstOrders.NextCursor == nil {
		t.Fatalf("first order page = (%+v, %v)", firstOrders, err)
	}
	secondOrders, err := repository.ListOrders(ctx, actor, commerce.PageRequest{Limit: 2, Cursor: firstOrders.NextCursor})
	if err != nil || len(secondOrders.Orders) != 1 || secondOrders.NextCursor != nil {
		t.Fatalf("second order page = (%+v, %v)", secondOrders, err)
	}
	seenOrders := make(map[uuid.UUID]struct{}, 3)
	for _, current := range append(firstOrders.Orders, secondOrders.Orders...) {
		if _, duplicate := seenOrders[current.ID]; duplicate {
			t.Fatalf("duplicate order across keyset pages: %s", current.ID)
		}
		seenOrders[current.ID] = struct{}{}
	}
}

func openOrderRepositoryIntegration(t *testing.T) (context.Context, *pgxpool.Pool, *OrderRepository) {
	t.Helper()
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	pool, err := Open(ctx, databaseConfig(databaseURL))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(pool.Close)
	return ctx, pool, NewOrderRepository(pool, 5*time.Second, 10*time.Second, 2*time.Second)
}

func createCommerceActor(t *testing.T, ctx context.Context, pool *pgxpool.Pool, role string) uuid.UUID {
	t.Helper()
	userID, accountID := uuid.New(), uuid.New()
	email := fmt.Sprintf("commerce-%s@example.com", uuid.NewString())
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin commerce actor transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `
		INSERT INTO users (id, email, password_hash, display_name, role, status)
		VALUES ($1, $2, '$argon2id$integration-test', 'Commerce Test', $3, 'active')
	`, userID, email, role); err != nil {
		t.Fatalf("create commerce actor: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO accounts (id, user_id, currency) VALUES ($1, $2, 'USD')
	`, accountID, userID); err != nil {
		t.Fatalf("create commerce account: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit commerce actor: %v", err)
	}
	return userID
}

func createIntegrationProduct(
	t *testing.T,
	ctx context.Context,
	repository *OrderRepository,
	adminID, productID uuid.UUID,
	sku string,
	price, quantity int64,
) {
	t.Helper()
	if _, err := repository.CreateProduct(ctx, adminID, commerce.CreateProductParams{
		ProductID: productID,
		Request: commerce.CreateProductRequest{
			SKU: sku, Name: sku + " Product", PriceAmount: price,
			Currency: commerce.CurrencyUSD, Status: commerce.ProductStatusActive, InitialQuantity: quantity,
		},
	}); err != nil {
		t.Fatalf("create integration product %s: %v", sku, err)
	}
}

func integrationOrderParams(productA, productB uuid.UUID, quantityA, quantityB int64) commerce.CreateOrderParams {
	items := []commerce.RequestedItem{
		{ItemID: uuid.New(), ProductID: productA, Quantity: quantityA, ExpectedProductVersion: 1},
		{ItemID: uuid.New(), ProductID: productB, Quantity: quantityB, ExpectedProductVersion: 1},
	}
	sort.Slice(items, func(i, j int) bool {
		return bytes.Compare(items[i].ProductID[:], items[j].ProductID[:]) < 0
	})
	return repositoryOrderParams(commerce.CreateOrderParams{OrderID: uuid.New(), Items: items})
}

func repositoryOrderParams(params commerce.CreateOrderParams) commerce.CreateOrderParams {
	keyHash := idempotency.KeyHash(uuid.NewString())
	requestHash := idempotency.RequestHash(idempotency.OrderCreateOperation, params.OrderID[:])
	params.KeyHash = keyHash[:]
	params.RequestHash = requestHash[:]
	return params
}

func idempotentOrderParams(productID uuid.UUID, quantity int64, key, semanticRequest string) commerce.CreateOrderParams {
	keyHash := idempotency.KeyHash(key)
	requestHash := idempotency.RequestHash(idempotency.OrderCreateOperation, []byte(semanticRequest))
	return commerce.CreateOrderParams{
		OrderID: uuid.New(), KeyHash: keyHash[:], RequestHash: requestHash[:],
		Items: []commerce.RequestedItem{{
			ItemID: uuid.New(), ProductID: productID, Quantity: quantity, ExpectedProductVersion: 1,
		}},
	}
}

func uniqueIntegrationSKU(prefix string) string {
	return prefix + "-" + strings.ToUpper(uuid.NewString()[:8])
}

func assertUniqueProductPages(
	t *testing.T,
	first, second []commerce.Product,
	wantIDs []uuid.UUID,
) {
	t.Helper()
	seen := make(map[uuid.UUID]struct{}, len(wantIDs))
	for _, product := range append(first, second...) {
		if _, duplicate := seen[product.ID]; duplicate {
			t.Fatalf("duplicate product across keyset pages: %s", product.ID)
		}
		seen[product.ID] = struct{}{}
	}
	for _, wantID := range wantIDs {
		if _, ok := seen[wantID]; !ok {
			t.Fatalf("product %s omitted from keyset pages", wantID)
		}
	}
}

func registerCommerceCleanup(
	t *testing.T,
	pool *pgxpool.Pool,
	userIDs, productIDs []uuid.UUID,
) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := pool.Exec(ctx, `DELETE FROM orders WHERE user_id = ANY($1::uuid[])`, userIDs); err != nil {
			t.Errorf("delete integration orders: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM inventories WHERE product_id = ANY($1::uuid[])`, productIDs); err != nil {
			t.Errorf("delete integration inventory: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM products WHERE id = ANY($1::uuid[])`, productIDs); err != nil {
			t.Errorf("delete integration products: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM users WHERE id = ANY($1::uuid[])`, userIDs); err != nil {
			t.Errorf("delete integration actors: %v", err)
		}
	})
}

func assertInventoryQuantity(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	productID uuid.UUID,
	want int64,
) {
	t.Helper()
	var quantity int64
	if err := pool.QueryRow(ctx, `SELECT quantity FROM inventories WHERE product_id = $1`, productID).Scan(&quantity); err != nil {
		t.Fatalf("query inventory quantity: %v", err)
	}
	if quantity != want {
		t.Fatalf("inventory quantity = %d, want %d", quantity, want)
	}
}

func assertInventoryVersion(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	productID uuid.UUID,
	want int64,
) {
	t.Helper()
	var version int64
	if err := pool.QueryRow(ctx, `SELECT version FROM inventories WHERE product_id = $1`, productID).Scan(&version); err != nil {
		t.Fatalf("query inventory version: %v", err)
	}
	if version != want {
		t.Fatalf("inventory version = %d, want %d", version, want)
	}
}

func assertCommerceCounts(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	userID uuid.UUID,
	wantOrders, wantItems int,
) {
	t.Helper()
	var orders, items int
	if err := pool.QueryRow(ctx, `
		SELECT count(*), (SELECT count(*) FROM order_items oi JOIN orders o ON o.id = oi.order_id WHERE o.user_id = $1)
		FROM orders WHERE user_id = $1
	`, userID).Scan(&orders, &items); err != nil {
		t.Fatalf("count commerce rows: %v", err)
	}
	if orders != wantOrders || items != wantItems {
		t.Fatalf("commerce row counts = orders:%d items:%d, want %d/%d", orders, items, wantOrders, wantItems)
	}
}

func assertIdempotencyRows(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	actorID uuid.UUID,
	operation string,
	want int,
) {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM idempotency_keys WHERE actor_id = $1 AND operation = $2
	`, actorID, operation).Scan(&count); err != nil {
		t.Fatalf("count idempotency rows: %v", err)
	}
	if count != want {
		t.Fatalf("idempotency row count = %d, want %d", count, want)
	}
}
