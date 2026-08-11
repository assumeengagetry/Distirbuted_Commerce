package order

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestServiceCreateProductNormalizesInput(t *testing.T) {
	t.Parallel()
	var captured CreateProductParams
	repository := &stubRepository{createProduct: func(
		_ context.Context,
		_ uuid.UUID,
		params CreateProductParams,
	) (AdminProduct, error) {
		captured = params
		return AdminProduct{Product: Product{ID: params.ProductID}}, nil
	}}
	service := newOrderTestService(t, repository)
	actor := Actor{UserID: uuid.New(), Role: RoleAdmin}
	created, err := service.CreateProduct(t.Context(), actor, CreateProductRequest{
		SKU: " phase3.sku ", Name: " Product Name ", Description: " Description ",
		PriceAmount: 2500, Currency: " usd ", Status: ProductStatusActive, InitialQuantity: 4,
	})
	if err != nil {
		t.Fatalf("CreateProduct() error = %v", err)
	}
	if captured.Request.SKU != "PHASE3.SKU" || captured.Request.Name != "Product Name" ||
		captured.Request.Description != "Description" || captured.Request.Currency != CurrencyUSD {
		t.Fatalf("normalized request = %+v", captured.Request)
	}
	if captured.ProductID == uuid.Nil || created.Product.ID != captured.ProductID {
		t.Fatalf("product IDs = captured:%s created:%s", captured.ProductID, created.Product.ID)
	}
}

func TestServiceProductAndInventoryValidation(t *testing.T) {
	t.Parallel()
	service := newOrderTestService(t, &stubRepository{})
	admin := Actor{UserID: uuid.New(), Role: RoleAdmin}
	productID := uuid.New()

	productTests := []struct {
		name    string
		request CreateProductRequest
		want    error
	}{
		{name: "SKU", request: CreateProductRequest{SKU: "x", Name: "Valid", PriceAmount: 1, Currency: CurrencyUSD, Status: ProductStatusActive}, want: ErrInvalidSKU},
		{name: "name", request: CreateProductRequest{SKU: "VALID", Name: " ", PriceAmount: 1, Currency: CurrencyUSD, Status: ProductStatusActive}, want: ErrInvalidProductName},
		{name: "price", request: CreateProductRequest{SKU: "VALID", Name: "Valid", PriceAmount: 0, Currency: CurrencyUSD, Status: ProductStatusActive}, want: ErrInvalidPrice},
		{name: "currency", request: CreateProductRequest{SKU: "VALID", Name: "Valid", PriceAmount: 1, Currency: "EUR", Status: ProductStatusActive}, want: ErrInvalidCurrency},
		{name: "status", request: CreateProductRequest{SKU: "VALID", Name: "Valid", PriceAmount: 1, Currency: CurrencyUSD, Status: "deleted"}, want: ErrInvalidProductStatus},
		{name: "inventory", request: CreateProductRequest{SKU: "VALID", Name: "Valid", PriceAmount: 1, Currency: CurrencyUSD, Status: ProductStatusActive, InitialQuantity: -1}, want: ErrInvalidInitialQuantity},
	}
	for _, test := range productTests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := service.CreateProduct(t.Context(), admin, test.request); !errors.Is(err, test.want) {
				t.Fatalf("CreateProduct() error = %v, want %v", err, test.want)
			}
		})
	}
	if _, err := service.UpdateProduct(t.Context(), admin, productID, UpdateProductRequest{
		ExpectedVersion: 1,
	}); !errors.Is(err, ErrEmptyProductUpdate) {
		t.Fatalf("UpdateProduct(empty) error = %v, want ErrEmptyProductUpdate", err)
	}
	if _, err := service.AdjustInventory(t.Context(), admin, productID, AdjustInventoryRequest{
		ExpectedVersion: 1,
	}); !errors.Is(err, ErrInvalidInventoryDelta) {
		t.Fatalf("AdjustInventory(zero) error = %v, want ErrInvalidInventoryDelta", err)
	}
	if _, err := service.CreateProduct(t.Context(), Actor{UserID: uuid.New(), Role: RoleCustomer}, CreateProductRequest{}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("customer CreateProduct() error = %v, want ErrForbidden", err)
	}
}

func TestServiceCreateOrderSortsAndAssignsItemIDs(t *testing.T) {
	t.Parallel()
	first := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	second := uuid.MustParse("ffffffff-ffff-ffff-ffff-ffffffffffff")
	var captured CreateOrderParams
	repository := &stubRepository{createOrder: func(
		_ context.Context,
		_ uuid.UUID,
		params CreateOrderParams,
	) (Order, error) {
		captured = params
		return Order{ID: params.OrderID}, nil
	}}
	service := newOrderTestService(t, repository)
	actor := Actor{UserID: uuid.New(), Role: RoleCustomer}
	created, err := service.CreateOrder(t.Context(), actor, CreateOrderRequest{IdempotencyKey: "sort-order-1", Items: []RequestedItem{
		{ProductID: second, Quantity: 2, ExpectedProductVersion: 3},
		{ProductID: first, Quantity: 1, ExpectedProductVersion: 4},
	}})
	if err != nil {
		t.Fatalf("CreateOrder() error = %v", err)
	}
	if captured.OrderID == uuid.Nil || created.ID != captured.OrderID {
		t.Fatalf("order IDs = captured:%s created:%s", captured.OrderID, created.ID)
	}
	if len(captured.Items) != 2 || captured.Items[0].ProductID != first || captured.Items[1].ProductID != second {
		t.Fatalf("sorted items = %+v", captured.Items)
	}
	if captured.Items[0].ItemID == uuid.Nil || captured.Items[1].ItemID == uuid.Nil ||
		captured.Items[0].ItemID == captured.Items[1].ItemID {
		t.Fatalf("item IDs = %s, %s", captured.Items[0].ItemID, captured.Items[1].ItemID)
	}
}

func TestServiceCreateOrderCanonicalHashIgnoresInputItemOrder(t *testing.T) {
	t.Parallel()
	first := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	second := uuid.MustParse("ffffffff-ffff-ffff-ffff-ffffffffffff")
	var captured []CreateOrderParams
	repository := &stubRepository{createOrder: func(
		_ context.Context,
		_ uuid.UUID,
		params CreateOrderParams,
	) (Order, error) {
		captured = append(captured, params)
		return Order{ID: params.OrderID}, nil
	}}
	service := newOrderTestService(t, repository)
	actor := Actor{UserID: uuid.New(), Role: RoleCustomer}
	requests := [][]RequestedItem{
		{{ProductID: first, Quantity: 1, ExpectedProductVersion: 2}, {ProductID: second, Quantity: 3, ExpectedProductVersion: 4}},
		{{ProductID: second, Quantity: 3, ExpectedProductVersion: 4}, {ProductID: first, Quantity: 1, ExpectedProductVersion: 2}},
	}
	for _, items := range requests {
		if _, err := service.CreateOrder(t.Context(), actor, CreateOrderRequest{
			Items: items, IdempotencyKey: "canonical-order-key",
		}); err != nil {
			t.Fatalf("CreateOrder() error = %v", err)
		}
	}
	if len(captured) != 2 || !bytes.Equal(captured[0].KeyHash, captured[1].KeyHash) ||
		!bytes.Equal(captured[0].RequestHash, captured[1].RequestHash) {
		t.Fatalf("canonical order hashes differ: %+v", captured)
	}
}

func TestServiceCreateOrderValidation(t *testing.T) {
	t.Parallel()
	service := newOrderTestService(t, &stubRepository{})
	actor := Actor{UserID: uuid.New(), Role: RoleCustomer}
	productID := uuid.New()
	tests := []struct {
		name  string
		actor Actor
		items []RequestedItem
		want  error
	}{
		{name: "wrong role", actor: Actor{UserID: uuid.New(), Role: RoleAdmin}, items: []RequestedItem{{ProductID: productID, Quantity: 1, ExpectedProductVersion: 1}}, want: ErrForbidden},
		{name: "empty", actor: actor, want: ErrInvalidOrderItems},
		{name: "nil product", actor: actor, items: []RequestedItem{{Quantity: 1, ExpectedProductVersion: 1}}, want: ErrInvalidOrderItems},
		{name: "duplicate", actor: actor, items: []RequestedItem{{ProductID: productID, Quantity: 1, ExpectedProductVersion: 1}, {ProductID: productID, Quantity: 1, ExpectedProductVersion: 1}}, want: ErrInvalidOrderItems},
		{name: "quantity", actor: actor, items: []RequestedItem{{ProductID: productID, Quantity: 0, ExpectedProductVersion: 1}}, want: ErrInvalidItemQuantity},
		{name: "version", actor: actor, items: []RequestedItem{{ProductID: productID, Quantity: 1}}, want: ErrInvalidExpectedVersion},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := service.CreateOrder(t.Context(), test.actor, CreateOrderRequest{
				Items: test.items, IdempotencyKey: "validation-key",
			}); !errors.Is(err, test.want) {
				t.Fatalf("CreateOrder() error = %v, want %v", err, test.want)
			}
		})
	}
	validItems := []RequestedItem{{ProductID: productID, Quantity: 1, ExpectedProductVersion: 1}}
	if _, err := service.CreateOrder(t.Context(), actor, CreateOrderRequest{Items: validItems}); !errors.Is(err, ErrIdempotencyKeyRequired) {
		t.Fatalf("CreateOrder(missing key) error = %v, want ErrIdempotencyKeyRequired", err)
	}
	if _, err := service.CreateOrder(t.Context(), actor, CreateOrderRequest{
		Items: validItems, IdempotencyKey: "invalid key",
	}); !errors.Is(err, ErrInvalidIdempotencyKey) {
		t.Fatalf("CreateOrder(invalid key) error = %v, want ErrInvalidIdempotencyKey", err)
	}
}

func newOrderTestService(t *testing.T, repository Repository) *Service {
	t.Helper()
	service, err := NewService(
		repository, slog.New(slog.NewJSONHandler(io.Discard, nil)), 5*time.Second,
	)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	return service
}

type stubRepository struct {
	createProduct func(context.Context, uuid.UUID, CreateProductParams) (AdminProduct, error)
	updateProduct func(context.Context, uuid.UUID, uuid.UUID, UpdateProductRequest) (AdminProduct, error)
	createOrder   func(context.Context, uuid.UUID, CreateOrderParams) (Order, error)
}

func (s *stubRepository) CreateProduct(ctx context.Context, actorID uuid.UUID, params CreateProductParams) (AdminProduct, error) {
	if s.createProduct == nil {
		return AdminProduct{}, nil
	}
	return s.createProduct(ctx, actorID, params)
}

func (s *stubRepository) UpdateProduct(ctx context.Context, actorID, productID uuid.UUID, request UpdateProductRequest) (AdminProduct, error) {
	if s.updateProduct == nil {
		return AdminProduct{}, nil
	}
	return s.updateProduct(ctx, actorID, productID, request)
}

func (s *stubRepository) GetProduct(context.Context, uuid.UUID) (Product, error) {
	return Product{}, nil
}

func (s *stubRepository) GetAdminProduct(context.Context, uuid.UUID, uuid.UUID) (AdminProduct, error) {
	return AdminProduct{}, nil
}

func (s *stubRepository) ListProducts(context.Context, PageRequest) (ProductPage, error) {
	return ProductPage{}, nil
}

func (s *stubRepository) AdjustInventory(context.Context, uuid.UUID, uuid.UUID, AdjustInventoryRequest) (Inventory, error) {
	return Inventory{}, nil
}

func (s *stubRepository) ListInventory(context.Context, uuid.UUID, PageRequest) (InventoryPage, error) {
	return InventoryPage{}, nil
}

func (s *stubRepository) CreateOrder(ctx context.Context, actorID uuid.UUID, params CreateOrderParams) (Order, error) {
	if s.createOrder == nil {
		return Order{}, nil
	}
	return s.createOrder(ctx, actorID, params)
}

func (s *stubRepository) ListOrders(context.Context, Actor, PageRequest) (OrderPage, error) {
	return OrderPage{}, nil
}

func (s *stubRepository) GetOrder(context.Context, Actor, uuid.UUID) (Order, error) {
	return Order{}, nil
}
