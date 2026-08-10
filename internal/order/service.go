package order

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

var skuPattern = regexp.MustCompile(`^[A-Z0-9][A-Z0-9._-]{2,63}$`)

type Service struct {
	repository Repository
	logger     *slog.Logger
	timeout    time.Duration
}

func NewService(repository Repository, logger *slog.Logger, timeout time.Duration) (*Service, error) {
	if repository == nil {
		return nil, fmt.Errorf("order repository is required")
	}
	if logger == nil {
		return nil, fmt.Errorf("logger is required")
	}
	if timeout <= 0 {
		return nil, fmt.Errorf("database timeout must be positive")
	}
	return &Service{repository: repository, logger: logger, timeout: timeout}, nil
}

func (s *Service) CreateProduct(ctx context.Context, actor Actor, request CreateProductRequest) (AdminProduct, error) {
	if err := validateActor(actor, RoleAdmin); err != nil {
		return AdminProduct{}, err
	}
	normalized, err := normalizeCreateProduct(request)
	if err != nil {
		return AdminProduct{}, err
	}
	productID, err := uuid.NewRandom()
	if err != nil {
		return AdminProduct{}, fmt.Errorf("generate product ID: %w", err)
	}

	operationCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	product, err := s.repository.CreateProduct(operationCtx, actor.UserID, CreateProductParams{
		ProductID: productID,
		Request:   normalized,
	})
	if err != nil {
		return AdminProduct{}, err
	}
	s.logger.InfoContext(ctx, "product created", slog.String("product_id", product.Product.ID.String()))
	return product, nil
}

func (s *Service) UpdateProduct(
	ctx context.Context,
	actor Actor,
	productID uuid.UUID,
	request UpdateProductRequest,
) (AdminProduct, error) {
	if err := validateActor(actor, RoleAdmin); err != nil {
		return AdminProduct{}, err
	}
	if productID == uuid.Nil {
		return AdminProduct{}, ErrProductNotFound
	}
	normalized, err := normalizeUpdateProduct(request)
	if err != nil {
		return AdminProduct{}, err
	}

	operationCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	product, err := s.repository.UpdateProduct(operationCtx, actor.UserID, productID, normalized)
	if err != nil {
		return AdminProduct{}, err
	}
	s.logger.InfoContext(ctx, "product updated", slog.String("product_id", productID.String()))
	return product, nil
}

func (s *Service) GetProduct(ctx context.Context, productID uuid.UUID) (Product, error) {
	if productID == uuid.Nil {
		return Product{}, ErrProductNotFound
	}
	operationCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	return s.repository.GetProduct(operationCtx, productID)
}

func (s *Service) GetAdminProduct(ctx context.Context, actor Actor, productID uuid.UUID) (AdminProduct, error) {
	if err := validateActor(actor, RoleAdmin); err != nil {
		return AdminProduct{}, err
	}
	if productID == uuid.Nil {
		return AdminProduct{}, ErrProductNotFound
	}
	operationCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	return s.repository.GetAdminProduct(operationCtx, actor.UserID, productID)
}

func (s *Service) ListProducts(ctx context.Context, page PageRequest) (ProductPage, error) {
	if err := validatePage(page); err != nil {
		return ProductPage{}, err
	}
	operationCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	return s.repository.ListProducts(operationCtx, page)
}

func (s *Service) AdjustInventory(
	ctx context.Context,
	actor Actor,
	productID uuid.UUID,
	request AdjustInventoryRequest,
) (Inventory, error) {
	if err := validateActor(actor, RoleAdmin); err != nil {
		return Inventory{}, err
	}
	if productID == uuid.Nil {
		return Inventory{}, ErrProductNotFound
	}
	if request.ExpectedVersion <= 0 {
		return Inventory{}, ErrInvalidExpectedVersion
	}
	if request.Delta == 0 || request.Delta < -MaxInventoryQuantity || request.Delta > MaxInventoryQuantity {
		return Inventory{}, ErrInvalidInventoryDelta
	}

	operationCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	inventory, err := s.repository.AdjustInventory(operationCtx, actor.UserID, productID, request)
	if err != nil {
		return Inventory{}, err
	}
	s.logger.InfoContext(ctx, "inventory adjusted", slog.String("product_id", productID.String()))
	return inventory, nil
}

func (s *Service) ListInventory(ctx context.Context, actor Actor, page PageRequest) (InventoryPage, error) {
	if err := validateActor(actor, RoleAdmin); err != nil {
		return InventoryPage{}, err
	}
	if err := validatePage(page); err != nil {
		return InventoryPage{}, err
	}
	operationCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	return s.repository.ListInventory(operationCtx, actor.UserID, page)
}

func (s *Service) CreateOrder(ctx context.Context, actor Actor, request CreateOrderRequest) (Order, error) {
	if err := validateActor(actor, RoleCustomer); err != nil {
		return Order{}, err
	}
	items, err := normalizeOrderItems(request.Items)
	if err != nil {
		return Order{}, err
	}
	orderID, err := uuid.NewRandom()
	if err != nil {
		return Order{}, fmt.Errorf("generate order ID: %w", err)
	}

	operationCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	created, err := s.repository.CreateOrder(operationCtx, actor.UserID, CreateOrderParams{OrderID: orderID, Items: items})
	if err != nil {
		return Order{}, err
	}
	s.logger.InfoContext(ctx, "order created", slog.String("order_id", orderID.String()), slog.String("user_id", actor.UserID.String()))
	return created, nil
}

func (s *Service) ListOrders(ctx context.Context, actor Actor, page PageRequest) (OrderPage, error) {
	if err := validateActor(actor, ""); err != nil {
		return OrderPage{}, err
	}
	if err := validatePage(page); err != nil {
		return OrderPage{}, err
	}
	operationCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	return s.repository.ListOrders(operationCtx, actor, page)
}

func (s *Service) GetOrder(ctx context.Context, actor Actor, orderID uuid.UUID) (Order, error) {
	if err := validateActor(actor, ""); err != nil {
		return Order{}, err
	}
	if orderID == uuid.Nil {
		return Order{}, ErrOrderNotFound
	}
	operationCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	return s.repository.GetOrder(operationCtx, actor, orderID)
}

func validateActor(actor Actor, requiredRole string) error {
	if actor.UserID == uuid.Nil || (actor.Role != RoleCustomer && actor.Role != RoleAdmin) {
		return ErrInvalidActor
	}
	if requiredRole != "" && actor.Role != requiredRole {
		return ErrForbidden
	}
	return nil
}

func validatePage(page PageRequest) error {
	if page.Limit < 1 || page.Limit > MaxPageSize {
		return ErrInvalidPage
	}
	if page.Cursor != nil && (page.Cursor.ID == uuid.Nil || page.Cursor.CreatedAt.IsZero()) {
		return ErrInvalidPage
	}
	return nil
}

func normalizeCreateProduct(request CreateProductRequest) (CreateProductRequest, error) {
	request.SKU = strings.ToUpper(strings.TrimSpace(request.SKU))
	request.Name = strings.TrimSpace(request.Name)
	request.Description = strings.TrimSpace(request.Description)
	request.Currency = strings.ToUpper(strings.TrimSpace(request.Currency))
	if !skuPattern.MatchString(request.SKU) {
		return CreateProductRequest{}, ErrInvalidSKU
	}
	if length := utf8.RuneCountInString(request.Name); length < 1 || length > 200 {
		return CreateProductRequest{}, ErrInvalidProductName
	}
	if utf8.RuneCountInString(request.Description) > 2000 {
		return CreateProductRequest{}, ErrInvalidDescription
	}
	if request.PriceAmount < 1 || request.PriceAmount > MaxPriceAmount {
		return CreateProductRequest{}, ErrInvalidPrice
	}
	if request.Currency != CurrencyUSD {
		return CreateProductRequest{}, ErrInvalidCurrency
	}
	if !request.Status.Valid() {
		return CreateProductRequest{}, ErrInvalidProductStatus
	}
	if request.InitialQuantity < 0 || request.InitialQuantity > MaxInventoryQuantity {
		return CreateProductRequest{}, ErrInvalidInitialQuantity
	}
	return request, nil
}

func normalizeUpdateProduct(request UpdateProductRequest) (UpdateProductRequest, error) {
	if request.ExpectedVersion <= 0 {
		return UpdateProductRequest{}, ErrInvalidExpectedVersion
	}
	if request.Name == nil && request.Description == nil && request.PriceAmount == nil && request.Status == nil {
		return UpdateProductRequest{}, ErrEmptyProductUpdate
	}
	if request.Name != nil {
		value := strings.TrimSpace(*request.Name)
		if length := utf8.RuneCountInString(value); length < 1 || length > 200 {
			return UpdateProductRequest{}, ErrInvalidProductName
		}
		request.Name = &value
	}
	if request.Description != nil {
		value := strings.TrimSpace(*request.Description)
		if utf8.RuneCountInString(value) > 2000 {
			return UpdateProductRequest{}, ErrInvalidDescription
		}
		request.Description = &value
	}
	if request.PriceAmount != nil && (*request.PriceAmount < 1 || *request.PriceAmount > MaxPriceAmount) {
		return UpdateProductRequest{}, ErrInvalidPrice
	}
	if request.Status != nil && !request.Status.Valid() {
		return UpdateProductRequest{}, ErrInvalidProductStatus
	}
	return request, nil
}

func normalizeOrderItems(items []RequestedItem) ([]RequestedItem, error) {
	if len(items) < 1 || len(items) > MaxOrderItems {
		return nil, ErrInvalidOrderItems
	}
	normalized := make([]RequestedItem, len(items))
	seen := make(map[uuid.UUID]struct{}, len(items))
	for index, item := range items {
		if item.ProductID == uuid.Nil {
			return nil, ErrInvalidOrderItems
		}
		if _, exists := seen[item.ProductID]; exists {
			return nil, ErrInvalidOrderItems
		}
		if item.Quantity < 1 || item.Quantity > MaxItemQuantity {
			return nil, ErrInvalidItemQuantity
		}
		if item.ExpectedProductVersion <= 0 {
			return nil, ErrInvalidExpectedVersion
		}
		itemID, err := uuid.NewRandom()
		if err != nil {
			return nil, fmt.Errorf("generate order item ID: %w", err)
		}
		seen[item.ProductID] = struct{}{}
		item.ItemID = itemID
		normalized[index] = item
	}
	sort.Slice(normalized, func(i, j int) bool {
		return bytes.Compare(normalized[i].ProductID[:], normalized[j].ProductID[:]) < 0
	})
	return normalized, nil
}
