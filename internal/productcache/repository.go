package productcache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/assumeengagetry/distributed-commerce/internal/order"
)

const (
	cacheSchemaVersion = 1
	productKeyPrefix   = "dc:v1:catalog:product:"
)

var (
	readScript = redis.NewScript(`
local generation = redis.call("GET", KEYS[2])
local value = redis.call("GET", KEYS[1])
return {value or false, generation or false}
`)
	conditionalSetScript = redis.NewScript(`
local generation = redis.call("GET", KEYS[2])
if not generation then
  redis.call("SET", KEYS[2], ARGV[1])
  generation = ARGV[1]
end
if generation ~= ARGV[1] then
  return 0
end
redis.call("SET", KEYS[1], ARGV[2], "PX", ARGV[3])
return 1
`)
	invalidateScript = redis.NewScript(`
local token_index = 1
for key_index = 1, #KEYS, 2 do
  redis.call("SET", KEYS[key_index], ARGV[token_index])
  redis.call("DEL", KEYS[key_index + 1])
  token_index = token_index + 1
end
return #KEYS / 2
`)
)

type cacheEntry struct {
	SchemaVersion int          `json:"schema_version"`
	Generation    string       `json:"generation"`
	Product       cacheProduct `json:"product"`
}

type cacheProduct struct {
	ID          *uuid.UUID           `json:"id"`
	SKU         *string              `json:"sku"`
	Name        *string              `json:"name"`
	Description *string              `json:"description"`
	PriceAmount *int64               `json:"price_amount"`
	Currency    *string              `json:"currency"`
	Status      *order.ProductStatus `json:"status"`
	Version     *int64               `json:"version"`
	Available   *bool                `json:"available"`
	CreatedAt   *time.Time           `json:"created_at"`
	UpdatedAt   *time.Time           `json:"updated_at"`
}

type Repository struct {
	next             order.Repository
	client           redis.UniversalClient
	logger           *slog.Logger
	ttl              time.Duration
	operationTimeout time.Duration
}

var _ order.Repository = (*Repository)(nil)

func NewRepository(
	next order.Repository,
	client redis.UniversalClient,
	logger *slog.Logger,
	ttl, operationTimeout time.Duration,
) (*Repository, error) {
	if next == nil {
		return nil, fmt.Errorf("order repository is required")
	}
	if client == nil {
		return nil, fmt.Errorf("Redis client is required")
	}
	if logger == nil {
		return nil, fmt.Errorf("logger is required")
	}
	if ttl <= 0 {
		return nil, fmt.Errorf("product cache TTL must be positive")
	}
	if operationTimeout <= 0 {
		return nil, fmt.Errorf("product cache operation timeout must be positive")
	}
	return &Repository{
		next:             next,
		client:           client,
		logger:           logger,
		ttl:              ttl,
		operationTimeout: operationTimeout,
	}, nil
}

func (r *Repository) CreateProduct(
	ctx context.Context,
	actorID uuid.UUID,
	params order.CreateProductParams,
) (order.AdminProduct, error) {
	product, err := r.next.CreateProduct(ctx, actorID, params)
	if err == nil {
		r.invalidateProducts(ctx, []uuid.UUID{params.ProductID})
	}
	return product, err
}

func (r *Repository) UpdateProduct(
	ctx context.Context,
	actorID, productID uuid.UUID,
	request order.UpdateProductRequest,
) (order.AdminProduct, error) {
	product, err := r.next.UpdateProduct(ctx, actorID, productID, request)
	r.invalidateProducts(ctx, []uuid.UUID{productID})
	return product, err
}

func (r *Repository) GetProduct(ctx context.Context, productID uuid.UUID) (order.Product, error) {
	generation, cached, hit, canFill := r.readProduct(ctx, productID)
	if hit {
		return cached, nil
	}

	product, err := r.next.GetProduct(ctx, productID)
	if err != nil {
		return product, err
	}
	if !canFill || !validCachedProduct(product, productID) {
		return product, nil
	}
	if err := r.fillProduct(ctx, productID, generation, product); err != nil {
		r.logCacheFailure(ctx, "fill", productID, err)
	}
	return product, nil
}

func (r *Repository) GetAdminProduct(
	ctx context.Context,
	actorID, productID uuid.UUID,
) (order.AdminProduct, error) {
	return r.next.GetAdminProduct(ctx, actorID, productID)
}

func (r *Repository) ListProducts(ctx context.Context, page order.PageRequest) (order.ProductPage, error) {
	return r.next.ListProducts(ctx, page)
}

func (r *Repository) AdjustInventory(
	ctx context.Context,
	actorID, productID uuid.UUID,
	request order.AdjustInventoryRequest,
) (order.Inventory, error) {
	inventory, err := r.next.AdjustInventory(ctx, actorID, productID, request)
	r.invalidateProducts(ctx, []uuid.UUID{productID})
	return inventory, err
}

func (r *Repository) ListInventory(
	ctx context.Context,
	actorID uuid.UUID,
	page order.PageRequest,
) (order.InventoryPage, error) {
	return r.next.ListInventory(ctx, actorID, page)
}

func (r *Repository) CreateOrder(
	ctx context.Context,
	actorID uuid.UUID,
	params order.CreateOrderParams,
) (order.Order, error) {
	productIDs := make([]uuid.UUID, len(params.Items))
	for index, item := range params.Items {
		productIDs[index] = item.ProductID
	}
	created, err := r.next.CreateOrder(ctx, actorID, params)
	if err == nil || errors.Is(err, order.ErrOperationOutcomeUnknown) {
		r.invalidateProducts(ctx, productIDs)
	}
	return created, err
}

func (r *Repository) ListOrders(
	ctx context.Context,
	actor order.Actor,
	page order.PageRequest,
) (order.OrderPage, error) {
	return r.next.ListOrders(ctx, actor, page)
}

func (r *Repository) GetOrder(
	ctx context.Context,
	actor order.Actor,
	orderID uuid.UUID,
) (order.Order, error) {
	return r.next.GetOrder(ctx, actor, orderID)
}

func (r *Repository) readProduct(
	ctx context.Context,
	productID uuid.UUID,
) (string, order.Product, bool, bool) {
	candidate, err := uuid.NewRandom()
	if err != nil {
		r.logCacheFailure(ctx, "generate read token", productID, err)
		return "", order.Product{}, false, false
	}
	operationCtx, cancel := context.WithTimeout(ctx, r.operationTimeout)
	defer cancel()

	values, err := readScript.Run(
		operationCtx,
		r.client,
		[]string{valueKey(productID), generationKey(productID)},
		candidate.String(),
	).Slice()
	if err != nil {
		r.logCacheFailure(ctx, "read", productID, err)
		return "", order.Product{}, false, false
	}
	if len(values) != 2 {
		r.logCacheFailure(ctx, "read", productID, fmt.Errorf("unexpected Lua result length %d", len(values)))
		return "", order.Product{}, false, false
	}
	if values[1] == nil {
		return candidate.String(), order.Product{}, false, true
	}

	generation, err := decodeGeneration(values[1])
	if err != nil {
		r.logCacheFailure(ctx, "read generation", productID, err)
		return "", order.Product{}, false, false
	}
	if values[0] == nil {
		return generation, order.Product{}, false, true
	}

	encoded, ok := redisString(values[0])
	if !ok {
		r.logCacheFailure(ctx, "decode", productID, fmt.Errorf("value has type %T", values[0]))
		return generation, order.Product{}, false, true
	}
	entry, err := decodeCacheEntry(encoded)
	if err != nil {
		r.logCacheFailure(ctx, "decode", productID, err)
		return generation, order.Product{}, false, true
	}
	if entry.SchemaVersion != cacheSchemaVersion {
		r.logRejectedEntry(ctx, productID, "schema version mismatch")
		return generation, order.Product{}, false, true
	}
	if entry.Generation != generation {
		r.logRejectedEntry(ctx, productID, "generation mismatch")
		return generation, order.Product{}, false, true
	}
	product, ok := entry.Product.toProduct(productID)
	if !ok {
		r.logRejectedEntry(ctx, productID, "invalid product data")
		return generation, order.Product{}, false, true
	}
	return generation, product, true, true
}

func (r *Repository) fillProduct(
	ctx context.Context,
	productID uuid.UUID,
	generation string,
	product order.Product,
) error {
	encoded, err := json.Marshal(cacheEntry{
		SchemaVersion: cacheSchemaVersion,
		Generation:    generation,
		Product:       cacheProductFromProduct(product),
	})
	if err != nil {
		return fmt.Errorf("encode cache entry: %w", err)
	}

	operationCtx, cancel := context.WithTimeout(ctx, r.operationTimeout)
	defer cancel()
	result, err := conditionalSetScript.Run(
		operationCtx,
		r.client,
		[]string{valueKey(productID), generationKey(productID)},
		generation,
		encoded,
		ttlMilliseconds(r.ttl),
	).Int()
	if err != nil {
		return err
	}
	if result != 0 && result != 1 {
		return fmt.Errorf("unexpected conditional SET result %d", result)
	}
	return nil
}

func (r *Repository) invalidateProducts(ctx context.Context, productIDs []uuid.UUID) {
	if len(productIDs) == 0 {
		return
	}
	seen := make(map[uuid.UUID]struct{}, len(productIDs))
	keys := make([]string, 0, len(productIDs)*2)
	tokens := make([]any, 0, len(productIDs))
	for _, productID := range productIDs {
		if _, exists := seen[productID]; exists {
			continue
		}
		seen[productID] = struct{}{}
		token, err := uuid.NewRandom()
		if err != nil {
			r.logCacheFailure(ctx, "generate invalidation token", productID, err)
			return
		}
		keys = append(keys, generationKey(productID), valueKey(productID))
		tokens = append(tokens, token.String())
	}
	if len(keys) == 0 {
		return
	}
	operationCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.operationTimeout)
	defer cancel()
	if err := invalidateScript.Run(operationCtx, r.client, keys, tokens...).Err(); err != nil {
		r.logger.WarnContext(operationCtx, "product cache operation failed",
			slog.String("operation", "invalidate"), slog.Int("product_count", len(keys)/2), slog.Any("error", err))
	}
}

func (r *Repository) logCacheFailure(ctx context.Context, operation string, productID uuid.UUID, err error) {
	r.logger.WarnContext(
		ctx,
		"product cache operation failed",
		slog.String("operation", operation),
		slog.String("product_id", productID.String()),
		slog.Any("error", err),
	)
}

func (r *Repository) logRejectedEntry(ctx context.Context, productID uuid.UUID, reason string) {
	r.logger.WarnContext(
		ctx,
		"product cache entry rejected",
		slog.String("product_id", productID.String()),
		slog.String("reason", reason),
	)
}

func valueKey(productID uuid.UUID) string {
	return productKeyPrefix + productID.String() + ":value"
}

func generationKey(productID uuid.UUID) string {
	return productKeyPrefix + productID.String() + ":generation"
}

func decodeGeneration(value any) (string, error) {
	if value == nil {
		return "", fmt.Errorf("generation is missing")
	}
	encoded, ok := redisString(value)
	if !ok {
		return "", fmt.Errorf("generation has type %T", value)
	}
	generation, err := uuid.Parse(encoded)
	if err != nil || generation == uuid.Nil || generation.String() != encoded {
		return "", fmt.Errorf("invalid generation token")
	}
	return encoded, nil
}

func decodeCacheEntry(encoded string) (cacheEntry, error) {
	decoder := json.NewDecoder(strings.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var entry cacheEntry
	if err := decoder.Decode(&entry); err != nil {
		return cacheEntry{}, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return cacheEntry{}, fmt.Errorf("multiple JSON values")
		}
		return cacheEntry{}, fmt.Errorf("decode trailing cache data: %w", err)
	}
	return entry, nil
}

func redisString(value any) (string, bool) {
	switch value := value.(type) {
	case string:
		return value, true
	case []byte:
		return string(value), true
	default:
		return "", false
	}
}

func ttlMilliseconds(ttl time.Duration) int64 {
	milliseconds := ttl / time.Millisecond
	if ttl%time.Millisecond != 0 {
		milliseconds++
	}
	return max(int64(milliseconds), 1)
}

func cacheProductFromProduct(product order.Product) cacheProduct {
	return cacheProduct{
		ID:          &product.ID,
		SKU:         &product.SKU,
		Name:        &product.Name,
		Description: &product.Description,
		PriceAmount: &product.PriceAmount,
		Currency:    &product.Currency,
		Status:      &product.Status,
		Version:     &product.Version,
		Available:   &product.Available,
		CreatedAt:   &product.CreatedAt,
		UpdatedAt:   &product.UpdatedAt,
	}
}

func (product cacheProduct) toProduct(productID uuid.UUID) (order.Product, bool) {
	if product.ID == nil || product.SKU == nil || product.Name == nil || product.Description == nil ||
		product.PriceAmount == nil || product.Currency == nil || product.Status == nil || product.Version == nil ||
		product.Available == nil || product.CreatedAt == nil || product.UpdatedAt == nil {
		return order.Product{}, false
	}
	decoded := order.Product{
		ID:          *product.ID,
		SKU:         *product.SKU,
		Name:        *product.Name,
		Description: *product.Description,
		PriceAmount: *product.PriceAmount,
		Currency:    *product.Currency,
		Status:      *product.Status,
		Version:     *product.Version,
		Available:   *product.Available,
		CreatedAt:   *product.CreatedAt,
		UpdatedAt:   *product.UpdatedAt,
	}
	return decoded, validCachedProduct(decoded, productID)
}

func validCachedProduct(product order.Product, productID uuid.UUID) bool {
	if product.ID == uuid.Nil || product.ID != productID || product.Status != order.ProductStatusActive || product.Version <= 0 ||
		product.PriceAmount < 1 || product.PriceAmount > order.MaxPriceAmount || product.Currency != order.CurrencyUSD ||
		product.CreatedAt.IsZero() || product.UpdatedAt.IsZero() || product.UpdatedAt.Before(product.CreatedAt) {
		return false
	}
	if !utf8.ValidString(product.SKU) || !utf8.ValidString(product.Name) || !utf8.ValidString(product.Description) ||
		strings.TrimSpace(product.SKU) != product.SKU || strings.TrimSpace(product.Name) != product.Name ||
		strings.TrimSpace(product.Description) != product.Description {
		return false
	}
	if len(product.SKU) < 3 || len(product.SKU) > 64 || utf8.RuneCountInString(product.Name) < 1 ||
		utf8.RuneCountInString(product.Name) > 200 || utf8.RuneCountInString(product.Description) > 2000 {
		return false
	}
	for index := range len(product.SKU) {
		character := product.SKU[index]
		if character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' ||
			index > 0 && (character == '.' || character == '_' || character == '-') {
			continue
		}
		return false
	}
	return true
}
