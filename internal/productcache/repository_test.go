package productcache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/assumeengagetry/distributed-commerce/internal/order"
)

const testOperationTimeout = 250 * time.Millisecond

func TestNewRepositoryValidatesInputs(t *testing.T) {
	t.Parallel()
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	logger := discardLogger()
	next := &fakeRepository{}

	tests := []struct {
		name             string
		next             order.Repository
		client           redis.UniversalClient
		logger           *slog.Logger
		ttl              time.Duration
		operationTimeout time.Duration
	}{
		{name: "repository", client: client, logger: logger, ttl: time.Minute, operationTimeout: time.Second},
		{name: "client", next: next, logger: logger, ttl: time.Minute, operationTimeout: time.Second},
		{name: "logger", next: next, client: client, ttl: time.Minute, operationTimeout: time.Second},
		{name: "zero TTL", next: next, client: client, logger: logger, operationTimeout: time.Second},
		{name: "negative TTL", next: next, client: client, logger: logger, ttl: -time.Second, operationTimeout: time.Second},
		{name: "zero operation timeout", next: next, client: client, logger: logger, ttl: time.Minute},
		{name: "negative operation timeout", next: next, client: client, logger: logger, ttl: time.Minute, operationTimeout: -time.Second},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if repository, err := NewRepository(
				test.next, test.client, test.logger, test.ttl, test.operationTimeout,
			); err == nil || repository != nil {
				t.Fatalf("NewRepository() = (%v, %v), want (nil, error)", repository, err)
			}
		})
	}
	if _, err := NewRepository(next, client, logger, time.Minute, time.Second); err != nil {
		t.Fatalf("NewRepository(valid) error = %v", err)
	}
}

func TestGetProductMissFillAndHit(t *testing.T) {
	t.Parallel()
	product := testProduct(uuid.New(), "cached", order.ProductStatusActive)
	var calls int
	next := &fakeRepository{getProduct: func(context.Context, uuid.UUID) (order.Product, error) {
		calls++
		return product, nil
	}}
	repository, server := newTestRepository(t, next, time.Minute)

	first, err := repository.GetProduct(t.Context(), product.ID)
	if err != nil {
		t.Fatalf("GetProduct(miss) error = %v", err)
	}
	if !reflect.DeepEqual(first, product) {
		t.Fatalf("GetProduct(miss) = %+v, want %+v", first, product)
	}
	encoded, err := server.Get(valueKey(product.ID))
	if err != nil {
		t.Fatalf("cache value after miss: %v", err)
	}
	var entry cacheEntry
	if err := json.Unmarshal([]byte(encoded), &entry); err != nil {
		t.Fatalf("decode cache entry: %v", err)
	}
	generation := assertGenerationToken(t, server, product.ID)
	decoded, valid := entry.Product.toProduct(product.ID)
	if entry.SchemaVersion != cacheSchemaVersion || entry.Generation != generation || !valid || !reflect.DeepEqual(decoded, product) {
		t.Fatalf("cache entry = %+v", entry)
	}

	second, err := repository.GetProduct(t.Context(), product.ID)
	if err != nil {
		t.Fatalf("GetProduct(hit) error = %v", err)
	}
	if !reflect.DeepEqual(second, product) {
		t.Fatalf("GetProduct(hit) = %+v, want %+v", second, product)
	}
	if calls != 1 {
		t.Fatalf("backing GetProduct calls = %d, want 1", calls)
	}
}

func TestGetProductTTL(t *testing.T) {
	t.Parallel()
	const ttl = time.Minute
	product := testProduct(uuid.New(), "ttl", order.ProductStatusActive)
	var calls int
	next := &fakeRepository{getProduct: func(context.Context, uuid.UUID) (order.Product, error) {
		calls++
		return product, nil
	}}
	repository, server := newTestRepository(t, next, ttl)

	if _, err := repository.GetProduct(t.Context(), product.ID); err != nil {
		t.Fatalf("GetProduct(first) error = %v", err)
	}
	if got := server.TTL(valueKey(product.ID)); got != ttl {
		t.Fatalf("cache TTL = %s, want %s", got, ttl)
	}
	server.FastForward(ttl - time.Second)
	if _, err := repository.GetProduct(t.Context(), product.ID); err != nil {
		t.Fatalf("GetProduct(before expiry) error = %v", err)
	}
	if calls != 1 {
		t.Fatalf("backing calls before expiry = %d, want 1", calls)
	}
	server.FastForward(2 * time.Second)
	if _, err := repository.GetProduct(t.Context(), product.ID); err != nil {
		t.Fatalf("GetProduct(after expiry) error = %v", err)
	}
	if calls != 2 {
		t.Fatalf("backing calls after expiry = %d, want 2", calls)
	}
}

func TestGetProductRejectedEntriesFallBackAndRepair(t *testing.T) {
	t.Parallel()
	productID := uuid.New()
	fresh := testProduct(productID, "fresh", order.ProductStatusActive)
	otherID := uuid.New()
	generation := uuid.NewString()
	otherGeneration := uuid.NewString()
	validEntry := cacheEntry{
		SchemaVersion: cacheSchemaVersion,
		Generation:    generation,
		Product:       cacheProductFromProduct(fresh),
	}

	tests := []struct {
		name       string
		entry      string
		generation string
	}{
		{name: "malformed JSON", entry: `{`, generation: generation},
		{name: "trailing JSON", entry: encodedEntry(t, validEntry) + `{}`, generation: generation},
		{name: "unknown field", entry: entryWithUnknownField(t, validEntry), generation: generation},
		{name: "omitted availability", entry: entryWithoutProductField(t, validEntry, "available"), generation: generation},
		{name: "schema mismatch", entry: encodedEntry(t, cacheEntry{SchemaVersion: 2, Generation: generation, Product: cacheProductFromProduct(fresh)}), generation: generation},
		{name: "generation mismatch", entry: encodedEntry(t, cacheEntry{SchemaVersion: 1, Generation: otherGeneration, Product: cacheProductFromProduct(fresh)}), generation: generation},
		{name: "product ID mismatch", entry: encodedEntry(t, cacheEntry{SchemaVersion: 1, Generation: generation, Product: cacheProductFromProduct(testProduct(otherID, "wrong", order.ProductStatusActive))}), generation: generation},
		{name: "inactive product", entry: encodedEntry(t, cacheEntry{SchemaVersion: 1, Generation: generation, Product: cacheProductFromProduct(testProduct(productID, "inactive", order.ProductStatusInactive))}), generation: generation},
		{name: "invalid product data", entry: encodedEntry(t, cacheEntry{SchemaVersion: 1, Generation: generation, Product: cacheProductFromProduct(order.Product{ID: productID, Status: order.ProductStatusActive})}), generation: generation},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var calls int
			next := &fakeRepository{getProduct: func(context.Context, uuid.UUID) (order.Product, error) {
				calls++
				return fresh, nil
			}}
			repository, server := newTestRepository(t, next, time.Minute)
			if err := server.Set(valueKey(productID), test.entry); err != nil {
				t.Fatalf("seed value: %v", err)
			}
			if err := server.Set(generationKey(productID), test.generation); err != nil {
				t.Fatalf("seed generation: %v", err)
			}

			got, err := repository.GetProduct(t.Context(), productID)
			if err != nil {
				t.Fatalf("GetProduct(rejected) error = %v", err)
			}
			if !reflect.DeepEqual(got, fresh) {
				t.Fatalf("GetProduct(rejected) = %+v, want %+v", got, fresh)
			}
			if _, err := repository.GetProduct(t.Context(), productID); err != nil {
				t.Fatalf("GetProduct(repaired) error = %v", err)
			}
			if calls != 1 {
				t.Fatalf("backing calls = %d, want 1 after repaired hit", calls)
			}
		})
	}
}

func TestGetProductMalformedGenerationFallsBackWithoutUnsafeFill(t *testing.T) {
	t.Parallel()
	product := testProduct(uuid.New(), "generation", order.ProductStatusActive)
	var calls int
	next := &fakeRepository{getProduct: func(context.Context, uuid.UUID) (order.Product, error) {
		calls++
		return product, nil
	}}
	repository, server := newTestRepository(t, next, time.Minute)
	if err := server.Set(generationKey(product.ID), "legacy-counter"); err != nil {
		t.Fatalf("seed generation: %v", err)
	}

	for range 2 {
		got, err := repository.GetProduct(t.Context(), product.ID)
		if err != nil || !reflect.DeepEqual(got, product) {
			t.Fatalf("GetProduct() = (%+v, %v), want (%+v, nil)", got, err, product)
		}
	}
	if calls != 2 {
		t.Fatalf("backing calls = %d, want 2", calls)
	}
	if server.Exists(valueKey(product.ID)) {
		t.Fatal("cache value exists despite unknown generation")
	}
}

func TestGetProductRedisOutageFallsBack(t *testing.T) {
	t.Parallel()
	product := testProduct(uuid.New(), "outage", order.ProductStatusActive)
	wantErr := errors.New("database result")
	want := order.Product{ID: product.ID, Name: "partial result"}
	next := &fakeRepository{getProduct: func(context.Context, uuid.UUID) (order.Product, error) {
		return want, wantErr
	}}
	repository, server := newTestRepository(t, next, time.Minute)
	server.Close()

	got, err := repository.GetProduct(t.Context(), product.ID)
	if !errors.Is(err, wantErr) || !reflect.DeepEqual(got, want) {
		t.Fatalf("GetProduct() = (%+v, %v), want (%+v, %v)", got, err, want, wantErr)
	}
}

func TestGetProductDoesNotNegativeCache(t *testing.T) {
	t.Parallel()
	product := testProduct(uuid.New(), "eventual", order.ProductStatusActive)
	var calls int
	next := &fakeRepository{getProduct: func(context.Context, uuid.UUID) (order.Product, error) {
		calls++
		if calls == 1 {
			return order.Product{}, order.ErrProductNotFound
		}
		return product, nil
	}}
	repository, server := newTestRepository(t, next, time.Minute)

	if _, err := repository.GetProduct(t.Context(), product.ID); !errors.Is(err, order.ErrProductNotFound) {
		t.Fatalf("GetProduct(first) error = %v, want ErrProductNotFound", err)
	}
	if server.Exists(valueKey(product.ID)) {
		t.Fatal("negative result was cached")
	}
	if server.Exists(generationKey(product.ID)) {
		t.Fatal("negative result created a persistent generation")
	}
	if _, err := repository.GetProduct(t.Context(), product.ID); err != nil {
		t.Fatalf("GetProduct(second) error = %v", err)
	}
	if _, err := repository.GetProduct(t.Context(), product.ID); err != nil {
		t.Fatalf("GetProduct(third) error = %v", err)
	}
	if calls != 2 {
		t.Fatalf("backing calls = %d, want 2", calls)
	}
}

func TestCreateProductInvalidatesOnlyOnSuccess(t *testing.T) {
	t.Parallel()
	actorID := uuid.New()

	t.Run("success", func(t *testing.T) {
		t.Parallel()
		product := testProduct(uuid.New(), "created", order.ProductStatusActive)
		want := order.AdminProduct{Product: product}
		next := &fakeRepository{createProduct: func(
			context.Context, uuid.UUID, order.CreateProductParams,
		) (order.AdminProduct, error) {
			return want, nil
		}}
		repository, server := newTestRepository(t, next, time.Minute)
		seedValue(t, server, product.ID)

		got, err := repository.CreateProduct(t.Context(), actorID, order.CreateProductParams{ProductID: product.ID})
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("CreateProduct() = (%+v, %v), want (%+v, nil)", got, err, want)
		}
		assertInvalidated(t, server, product.ID)
	})

	t.Run("error", func(t *testing.T) {
		t.Parallel()
		productID := uuid.New()
		wantErr := errors.New("create failed")
		next := &fakeRepository{createProduct: func(
			context.Context, uuid.UUID, order.CreateProductParams,
		) (order.AdminProduct, error) {
			return order.AdminProduct{}, wantErr
		}}
		repository, server := newTestRepository(t, next, time.Minute)
		seedValue(t, server, productID)

		if _, err := repository.CreateProduct(t.Context(), actorID, order.CreateProductParams{ProductID: productID}); !errors.Is(err, wantErr) {
			t.Fatalf("CreateProduct() error = %v, want %v", err, wantErr)
		}
		if !server.Exists(valueKey(productID)) || server.Exists(generationKey(productID)) {
			t.Fatal("failed CreateProduct invalidated cache")
		}
	})
}

func TestUpdateProductInvalidatesAfterSuccessAndError(t *testing.T) {
	t.Parallel()
	for _, repositoryErr := range []error{nil, errors.New("update failed")} {
		repositoryErr := repositoryErr
		name := "success"
		if repositoryErr != nil {
			name = "error"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			productID := uuid.New()
			want := order.AdminProduct{Product: testProduct(productID, name, order.ProductStatusActive)}
			next := &fakeRepository{updateProduct: func(
				context.Context, uuid.UUID, uuid.UUID, order.UpdateProductRequest,
			) (order.AdminProduct, error) {
				return want, repositoryErr
			}}
			repository, server := newTestRepository(t, next, time.Minute)
			seedValue(t, server, productID)

			got, err := repository.UpdateProduct(t.Context(), uuid.New(), productID, order.UpdateProductRequest{})
			if !errors.Is(err, repositoryErr) || !reflect.DeepEqual(got, want) {
				t.Fatalf("UpdateProduct() = (%+v, %v), want (%+v, %v)", got, err, want, repositoryErr)
			}
			assertInvalidated(t, server, productID)
		})
	}
}

func TestAdjustInventoryInvalidatesAfterSuccessAndError(t *testing.T) {
	t.Parallel()
	for _, repositoryErr := range []error{nil, errors.New("adjust failed")} {
		repositoryErr := repositoryErr
		name := "success"
		if repositoryErr != nil {
			name = "error"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			productID := uuid.New()
			want := order.Inventory{ProductID: productID, Quantity: 7}
			next := &fakeRepository{adjustInventory: func(
				context.Context, uuid.UUID, uuid.UUID, order.AdjustInventoryRequest,
			) (order.Inventory, error) {
				return want, repositoryErr
			}}
			repository, server := newTestRepository(t, next, time.Minute)
			seedValue(t, server, productID)

			got, err := repository.AdjustInventory(t.Context(), uuid.New(), productID, order.AdjustInventoryRequest{})
			if !errors.Is(err, repositoryErr) || !reflect.DeepEqual(got, want) {
				t.Fatalf("AdjustInventory() = (%+v, %v), want (%+v, %v)", got, err, want, repositoryErr)
			}
			assertInvalidated(t, server, productID)
		})
	}
}

func TestCreateOrderFailureDoesNotCreateCacheKeys(t *testing.T) {
	t.Parallel()
	first := uuid.New()
	second := uuid.New()
	want := order.Order{ID: uuid.New()}
	wantErr := errors.New("order failed")
	next := &fakeRepository{createOrder: func(
		context.Context, uuid.UUID, order.CreateOrderParams,
	) (order.Order, error) {
		return want, wantErr
	}}
	repository, server := newTestRepository(t, next, time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got, err := repository.CreateOrder(ctx, uuid.New(), order.CreateOrderParams{Items: []order.RequestedItem{
		{ProductID: first},
		{ProductID: second},
		{ProductID: first},
	}})
	if !errors.Is(err, wantErr) || !reflect.DeepEqual(got, want) {
		t.Fatalf("CreateOrder() = (%+v, %v), want (%+v, %v)", got, err, want, wantErr)
	}
	if keys := server.Keys(); len(keys) != 0 {
		t.Fatalf("failed CreateOrder created cache keys: %v", keys)
	}
}

func TestCreateOrderUnknownOutcomeInvalidatesProducts(t *testing.T) {
	t.Parallel()
	first := uuid.New()
	second := uuid.New()
	want := order.Order{ID: uuid.New()}
	next := &fakeRepository{createOrder: func(
		context.Context, uuid.UUID, order.CreateOrderParams,
	) (order.Order, error) {
		return want, fmt.Errorf("commit resolution: %w", order.ErrOperationOutcomeUnknown)
	}}
	repository, server := newTestRepository(t, next, time.Minute)
	seedValue(t, server, first)
	seedValue(t, server, second)

	got, err := repository.CreateOrder(t.Context(), uuid.New(), order.CreateOrderParams{Items: []order.RequestedItem{
		{ProductID: first},
		{ProductID: second},
	}})
	if !errors.Is(err, order.ErrOperationOutcomeUnknown) || !reflect.DeepEqual(got, want) {
		t.Fatalf("CreateOrder() = (%+v, %v), want (%+v, ErrOperationOutcomeUnknown)", got, err, want)
	}
	assertInvalidated(t, server, first)
	assertInvalidated(t, server, second)
}

func TestCreateOrderSuccessInvalidatesDistinctProductsInOneBatch(t *testing.T) {
	t.Parallel()
	first := uuid.New()
	second := uuid.New()
	want := order.Order{ID: uuid.New()}
	next := &fakeRepository{createOrder: func(
		context.Context, uuid.UUID, order.CreateOrderParams,
	) (order.Order, error) {
		return want, nil
	}}
	repository, server := newTestRepository(t, next, time.Minute)
	seedValue(t, server, first)
	seedValue(t, server, second)
	if _, err := server.Push(generationKey(first), "wrong-type"); err != nil {
		t.Fatalf("seed wrong-type generation: %v", err)
	}
	const malformedGeneration = "legacy-counter"
	if err := server.Set(generationKey(second), malformedGeneration); err != nil {
		t.Fatalf("seed malformed generation: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got, err := repository.CreateOrder(ctx, uuid.New(), order.CreateOrderParams{Items: []order.RequestedItem{
		{ProductID: first},
		{ProductID: second},
		{ProductID: first},
	}})
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("CreateOrder() = (%+v, %v), want (%+v, nil)", got, err, want)
	}
	assertInvalidated(t, server, first)
	secondGeneration := assertInvalidated(t, server, second)
	if secondGeneration == malformedGeneration {
		t.Fatal("invalidation retained malformed generation")
	}
}

func TestCacheFailureDoesNotChangeWriteResult(t *testing.T) {
	t.Parallel()
	productID := uuid.New()
	want := order.AdminProduct{Product: testProduct(productID, "result", order.ProductStatusActive)}
	wantErr := errors.New("repository result")
	next := &fakeRepository{updateProduct: func(
		context.Context, uuid.UUID, uuid.UUID, order.UpdateProductRequest,
	) (order.AdminProduct, error) {
		return want, wantErr
	}}
	repository, server := newTestRepository(t, next, time.Minute)
	server.Close()

	got, err := repository.UpdateProduct(t.Context(), uuid.New(), productID, order.UpdateProductRequest{})
	if !errors.Is(err, wantErr) || !reflect.DeepEqual(got, want) {
		t.Fatalf("UpdateProduct() = (%+v, %v), want (%+v, %v)", got, err, want, wantErr)
	}
}

func TestGenerationFencePreventsStaleFill(t *testing.T) {
	t.Parallel()
	productID := uuid.New()
	stale := testProduct(productID, "stale", order.ProductStatusActive)
	fresh := testProduct(productID, "fresh", order.ProductStatusActive)
	readStarted := make(chan struct{})
	releaseRead := make(chan struct{})

	var mu sync.Mutex
	current := stale
	getCalls := 0
	next := &fakeRepository{
		getProduct: func(context.Context, uuid.UUID) (order.Product, error) {
			mu.Lock()
			getCalls++
			call := getCalls
			result := current
			mu.Unlock()
			if call == 1 {
				close(readStarted)
				<-releaseRead
			}
			return result, nil
		},
		updateProduct: func(
			context.Context, uuid.UUID, uuid.UUID, order.UpdateProductRequest,
		) (order.AdminProduct, error) {
			mu.Lock()
			current = fresh
			mu.Unlock()
			return order.AdminProduct{Product: fresh}, nil
		},
	}
	repository, server := newTestRepository(t, next, time.Minute)

	result := make(chan order.Product, 1)
	errResult := make(chan error, 1)
	go func() {
		product, err := repository.GetProduct(context.Background(), productID)
		result <- product
		errResult <- err
	}()
	<-readStarted
	if server.Exists(generationKey(productID)) {
		t.Fatal("cache miss initialized generation before the database read completed")
	}
	if _, err := repository.UpdateProduct(t.Context(), uuid.New(), productID, order.UpdateProductRequest{}); err != nil {
		t.Fatalf("UpdateProduct() error = %v", err)
	}
	afterInvalidation := assertGenerationToken(t, server, productID)
	close(releaseRead)
	if err := <-errResult; err != nil {
		t.Fatalf("stale GetProduct() error = %v", err)
	}
	if got := <-result; !reflect.DeepEqual(got, stale) {
		t.Fatalf("in-flight GetProduct() = %+v, want stale snapshot %+v", got, stale)
	}
	if server.Exists(valueKey(productID)) {
		t.Fatal("stale fill recreated the invalidated cache value")
	}
	if generation := assertGenerationToken(t, server, productID); generation != afterInvalidation {
		t.Fatal("stale fill changed the invalidated generation")
	}

	got, err := repository.GetProduct(t.Context(), productID)
	if err != nil || !reflect.DeepEqual(got, fresh) {
		t.Fatalf("GetProduct(after race) = (%+v, %v), want (%+v, nil)", got, err, fresh)
	}
	if _, err := repository.GetProduct(t.Context(), productID); err != nil {
		t.Fatalf("GetProduct(fresh hit) error = %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if getCalls != 2 {
		t.Fatalf("backing GetProduct calls = %d, want 2", getCalls)
	}
}

func TestGenerationFenceSurvivesRedisGenerationLoss(t *testing.T) {
	t.Parallel()
	productID := uuid.New()
	stale := testProduct(productID, "stale", order.ProductStatusActive)
	fresh := testProduct(productID, "fresh", order.ProductStatusActive)
	staleReadStarted := make(chan struct{})
	releaseStaleRead := make(chan struct{})

	var mu sync.Mutex
	getCalls := 0
	next := &fakeRepository{getProduct: func(context.Context, uuid.UUID) (order.Product, error) {
		mu.Lock()
		getCalls++
		call := getCalls
		mu.Unlock()
		if call == 1 {
			close(staleReadStarted)
			<-releaseStaleRead
			return stale, nil
		}
		return fresh, nil
	}}
	repository, server := newTestRepository(t, next, time.Minute)

	staleResult := make(chan order.Product, 1)
	staleErr := make(chan error, 1)
	go func() {
		product, err := repository.GetProduct(context.Background(), productID)
		staleResult <- product
		staleErr <- err
	}()
	<-staleReadStarted
	if server.Exists(generationKey(productID)) {
		t.Fatal("cache miss initialized generation before the database read completed")
	}
	server.FlushAll()

	got, err := repository.GetProduct(t.Context(), productID)
	if err != nil || !reflect.DeepEqual(got, fresh) {
		t.Fatalf("GetProduct(after Redis loss) = (%+v, %v), want (%+v, nil)", got, err, fresh)
	}
	reinitializedGeneration := assertGenerationToken(t, server, productID)

	close(releaseStaleRead)
	if err := <-staleErr; err != nil {
		t.Fatalf("stale GetProduct() error = %v", err)
	}
	if got := <-staleResult; !reflect.DeepEqual(got, stale) {
		t.Fatalf("in-flight GetProduct() = %+v, want stale snapshot %+v", got, stale)
	}
	got, err = repository.GetProduct(t.Context(), productID)
	if err != nil || !reflect.DeepEqual(got, fresh) {
		t.Fatalf("GetProduct(final hit) = (%+v, %v), want (%+v, nil)", got, err, fresh)
	}
	if generation := assertGenerationToken(t, server, productID); generation != reinitializedGeneration {
		t.Fatal("stale fill changed the reinitialized generation")
	}
	mu.Lock()
	defer mu.Unlock()
	if getCalls != 2 {
		t.Fatalf("backing GetProduct calls = %d, want 2", getCalls)
	}
}

func TestUncachedMethodsDelegateUnchanged(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	actorID := uuid.New()
	productID := uuid.New()
	actor := order.Actor{UserID: actorID, Role: order.RoleAdmin}
	page := order.PageRequest{Limit: 17}
	orderID := uuid.New()
	wantErr := errors.New("delegated error")
	next := &fakeRepository{
		getAdminProduct: func(gotCtx context.Context, gotActor, gotProduct uuid.UUID) (order.AdminProduct, error) {
			if gotCtx != ctx || gotActor != actorID || gotProduct != productID {
				t.Fatal("GetAdminProduct arguments changed")
			}
			return order.AdminProduct{}, wantErr
		},
		listProducts: func(gotCtx context.Context, gotPage order.PageRequest) (order.ProductPage, error) {
			if gotCtx != ctx || !reflect.DeepEqual(gotPage, page) {
				t.Fatal("ListProducts arguments changed")
			}
			return order.ProductPage{}, wantErr
		},
		listInventory: func(gotCtx context.Context, gotActor uuid.UUID, gotPage order.PageRequest) (order.InventoryPage, error) {
			if gotCtx != ctx || gotActor != actorID || !reflect.DeepEqual(gotPage, page) {
				t.Fatal("ListInventory arguments changed")
			}
			return order.InventoryPage{}, wantErr
		},
		listOrders: func(gotCtx context.Context, gotActor order.Actor, gotPage order.PageRequest) (order.OrderPage, error) {
			if gotCtx != ctx || !reflect.DeepEqual(gotActor, actor) || !reflect.DeepEqual(gotPage, page) {
				t.Fatal("ListOrders arguments changed")
			}
			return order.OrderPage{}, wantErr
		},
		getOrder: func(gotCtx context.Context, gotActor order.Actor, gotOrder uuid.UUID) (order.Order, error) {
			if gotCtx != ctx || !reflect.DeepEqual(gotActor, actor) || gotOrder != orderID {
				t.Fatal("GetOrder arguments changed")
			}
			return order.Order{}, wantErr
		},
	}
	repository, _ := newTestRepository(t, next, time.Minute)

	if _, err := repository.GetAdminProduct(ctx, actorID, productID); !errors.Is(err, wantErr) {
		t.Fatalf("GetAdminProduct() error = %v", err)
	}
	if _, err := repository.ListProducts(ctx, page); !errors.Is(err, wantErr) {
		t.Fatalf("ListProducts() error = %v", err)
	}
	if _, err := repository.ListInventory(ctx, actorID, page); !errors.Is(err, wantErr) {
		t.Fatalf("ListInventory() error = %v", err)
	}
	if _, err := repository.ListOrders(ctx, actor, page); !errors.Is(err, wantErr) {
		t.Fatalf("ListOrders() error = %v", err)
	}
	if _, err := repository.GetOrder(ctx, actor, orderID); !errors.Is(err, wantErr) {
		t.Fatalf("GetOrder() error = %v", err)
	}
}

func newTestRepository(t *testing.T, next order.Repository, ttl time.Duration) (*Repository, *miniredis.Miniredis) {
	t.Helper()
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{
		Addr:         server.Addr(),
		DialTimeout:  testOperationTimeout,
		ReadTimeout:  testOperationTimeout,
		WriteTimeout: testOperationTimeout,
		MaxRetries:   -1,
	})
	t.Cleanup(func() { _ = client.Close() })
	repository, err := NewRepository(next, client, discardLogger(), ttl, testOperationTimeout)
	if err != nil {
		t.Fatalf("NewRepository() error = %v", err)
	}
	return repository, server
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func testProduct(id uuid.UUID, name string, status order.ProductStatus) order.Product {
	createdAt := time.Date(2026, time.August, 10, 12, 0, 0, 123456000, time.UTC)
	return order.Product{
		ID:          id,
		SKU:         "CACHE.TEST",
		Name:        name,
		Description: "cache test product",
		PriceAmount: 1234,
		Currency:    order.CurrencyUSD,
		Status:      status,
		Version:     7,
		Available:   true,
		CreatedAt:   createdAt,
		UpdatedAt:   createdAt.Add(time.Minute),
	}
}

func encodedEntry(t *testing.T, entry cacheEntry) string {
	t.Helper()
	encoded, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("json.Marshal(cacheEntry) error = %v", err)
	}
	return string(encoded)
}

func entryWithUnknownField(t *testing.T, entry cacheEntry) string {
	t.Helper()
	decoded := decodedEntryMap(t, entry)
	product, ok := decoded["product"].(map[string]any)
	if !ok {
		t.Fatalf("encoded product has type %T", decoded["product"])
	}
	product["unexpected"] = true
	return encodedJSON(t, decoded)
}

func entryWithoutProductField(t *testing.T, entry cacheEntry, field string) string {
	t.Helper()
	decoded := decodedEntryMap(t, entry)
	product, ok := decoded["product"].(map[string]any)
	if !ok {
		t.Fatalf("encoded product has type %T", decoded["product"])
	}
	delete(product, field)
	return encodedJSON(t, decoded)
}

func decodedEntryMap(t *testing.T, entry cacheEntry) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal([]byte(encodedEntry(t, entry)), &decoded); err != nil {
		t.Fatalf("decode entry map: %v", err)
	}
	return decoded
}

func encodedJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode JSON: %v", err)
	}
	return string(encoded)
}

func seedValue(t *testing.T, server *miniredis.Miniredis, productID uuid.UUID) {
	t.Helper()
	entry := cacheEntry{
		SchemaVersion: cacheSchemaVersion,
		Generation:    uuid.NewString(),
		Product:       cacheProductFromProduct(testProduct(productID, "seed", order.ProductStatusActive)),
	}
	if err := server.Set(valueKey(productID), encodedEntry(t, entry)); err != nil {
		t.Fatalf("seed cache value: %v", err)
	}
}

func assertInvalidated(t *testing.T, server *miniredis.Miniredis, productID uuid.UUID) string {
	t.Helper()
	if server.Exists(valueKey(productID)) {
		t.Fatalf("cache value %q still exists", valueKey(productID))
	}
	return assertGenerationToken(t, server, productID)
}

func assertGenerationToken(t *testing.T, server *miniredis.Miniredis, productID uuid.UUID) string {
	t.Helper()
	generation, err := server.Get(generationKey(productID))
	if err != nil {
		t.Fatalf("read generation for %s: %v", productID, err)
	}
	parsed, err := uuid.Parse(generation)
	if err != nil || parsed == uuid.Nil || parsed.String() != generation {
		t.Fatalf("generation for %s is not a canonical token: %q", productID, generation)
	}
	return generation
}

type fakeRepository struct {
	createProduct   func(context.Context, uuid.UUID, order.CreateProductParams) (order.AdminProduct, error)
	updateProduct   func(context.Context, uuid.UUID, uuid.UUID, order.UpdateProductRequest) (order.AdminProduct, error)
	getProduct      func(context.Context, uuid.UUID) (order.Product, error)
	getAdminProduct func(context.Context, uuid.UUID, uuid.UUID) (order.AdminProduct, error)
	listProducts    func(context.Context, order.PageRequest) (order.ProductPage, error)
	adjustInventory func(context.Context, uuid.UUID, uuid.UUID, order.AdjustInventoryRequest) (order.Inventory, error)
	listInventory   func(context.Context, uuid.UUID, order.PageRequest) (order.InventoryPage, error)
	createOrder     func(context.Context, uuid.UUID, order.CreateOrderParams) (order.Order, error)
	listOrders      func(context.Context, order.Actor, order.PageRequest) (order.OrderPage, error)
	getOrder        func(context.Context, order.Actor, uuid.UUID) (order.Order, error)
}

func (f *fakeRepository) CreateProduct(
	ctx context.Context,
	actorID uuid.UUID,
	params order.CreateProductParams,
) (order.AdminProduct, error) {
	if f.createProduct == nil {
		return order.AdminProduct{}, nil
	}
	return f.createProduct(ctx, actorID, params)
}

func (f *fakeRepository) UpdateProduct(
	ctx context.Context,
	actorID, productID uuid.UUID,
	request order.UpdateProductRequest,
) (order.AdminProduct, error) {
	if f.updateProduct == nil {
		return order.AdminProduct{}, nil
	}
	return f.updateProduct(ctx, actorID, productID, request)
}

func (f *fakeRepository) GetProduct(ctx context.Context, productID uuid.UUID) (order.Product, error) {
	if f.getProduct == nil {
		return order.Product{}, nil
	}
	return f.getProduct(ctx, productID)
}

func (f *fakeRepository) GetAdminProduct(
	ctx context.Context,
	actorID, productID uuid.UUID,
) (order.AdminProduct, error) {
	if f.getAdminProduct == nil {
		return order.AdminProduct{}, nil
	}
	return f.getAdminProduct(ctx, actorID, productID)
}

func (f *fakeRepository) ListProducts(ctx context.Context, page order.PageRequest) (order.ProductPage, error) {
	if f.listProducts == nil {
		return order.ProductPage{}, nil
	}
	return f.listProducts(ctx, page)
}

func (f *fakeRepository) AdjustInventory(
	ctx context.Context,
	actorID, productID uuid.UUID,
	request order.AdjustInventoryRequest,
) (order.Inventory, error) {
	if f.adjustInventory == nil {
		return order.Inventory{}, nil
	}
	return f.adjustInventory(ctx, actorID, productID, request)
}

func (f *fakeRepository) ListInventory(
	ctx context.Context,
	actorID uuid.UUID,
	page order.PageRequest,
) (order.InventoryPage, error) {
	if f.listInventory == nil {
		return order.InventoryPage{}, nil
	}
	return f.listInventory(ctx, actorID, page)
}

func (f *fakeRepository) CreateOrder(
	ctx context.Context,
	actorID uuid.UUID,
	params order.CreateOrderParams,
) (order.Order, error) {
	if f.createOrder == nil {
		return order.Order{}, nil
	}
	return f.createOrder(ctx, actorID, params)
}

func (f *fakeRepository) ListOrders(
	ctx context.Context,
	actor order.Actor,
	page order.PageRequest,
) (order.OrderPage, error) {
	if f.listOrders == nil {
		return order.OrderPage{}, nil
	}
	return f.listOrders(ctx, actor, page)
}

func (f *fakeRepository) GetOrder(
	ctx context.Context,
	actor order.Actor,
	orderID uuid.UUID,
) (order.Order, error) {
	if f.getOrder == nil {
		return order.Order{}, nil
	}
	return f.getOrder(ctx, actor, orderID)
}
