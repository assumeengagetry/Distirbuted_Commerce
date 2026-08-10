package database

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	store "github.com/assumeengagetry/distributed-commerce/internal/database/sqlc"
	commerce "github.com/assumeengagetry/distributed-commerce/internal/order"
)

type OrderRepository struct {
	pool             *pgxpool.Pool
	queries          *store.Queries
	lockTimeout      time.Duration
	statementTimeout time.Duration
}

func NewOrderRepository(pool *pgxpool.Pool, lockTimeout, statementTimeout time.Duration) *OrderRepository {
	return &OrderRepository{
		pool: pool, queries: store.New(pool), lockTimeout: lockTimeout, statementTimeout: statementTimeout,
	}
}

func (r *OrderRepository) CreateProduct(
	ctx context.Context,
	actorID uuid.UUID,
	params commerce.CreateProductParams,
) (commerce.AdminProduct, error) {
	tx, queries, err := r.begin(ctx, "product creation")
	if err != nil {
		return commerce.AdminProduct{}, err
	}
	defer rollbackCommerceTransaction(tx)

	if err := authorizeCommerceActor(ctx, queries, actorID, commerce.RoleAdmin); err != nil {
		return commerce.AdminProduct{}, err
	}
	now, err := databaseTime(ctx, tx)
	if err != nil {
		return commerce.AdminProduct{}, mapCommerceDatabaseError("read product creation time", err)
	}
	dbProduct, err := queries.CreateProduct(ctx, store.CreateProductParams{
		ID: params.ProductID, Sku: params.Request.SKU, Name: params.Request.Name,
		Description: params.Request.Description, PriceAmount: params.Request.PriceAmount,
		Currency: params.Request.Currency, Status: string(params.Request.Status), CreatedAt: now,
	})
	if err != nil {
		return commerce.AdminProduct{}, mapProductWriteError(err)
	}
	dbInventory, err := queries.CreateInventory(ctx, store.CreateInventoryParams{
		ProductID: params.ProductID, Quantity: params.Request.InitialQuantity, CreatedAt: now,
	})
	if err != nil {
		return commerce.AdminProduct{}, mapCommerceDatabaseError("create product inventory", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return commerce.AdminProduct{}, mapCommerceDatabaseError("commit product creation", err)
	}
	return adminProductFromModels(dbProduct, dbInventory), nil
}

func (r *OrderRepository) UpdateProduct(
	ctx context.Context,
	actorID, productID uuid.UUID,
	request commerce.UpdateProductRequest,
) (commerce.AdminProduct, error) {
	tx, queries, err := r.begin(ctx, "product update")
	if err != nil {
		return commerce.AdminProduct{}, err
	}
	defer rollbackCommerceTransaction(tx)

	if err := authorizeCommerceActor(ctx, queries, actorID, commerce.RoleAdmin); err != nil {
		return commerce.AdminProduct{}, err
	}
	current, err := queries.GetProductForUpdate(ctx, productID)
	if errors.Is(err, pgx.ErrNoRows) {
		return commerce.AdminProduct{}, commerce.ErrProductNotFound
	}
	if err != nil {
		return commerce.AdminProduct{}, mapCommerceDatabaseError("lock product", err)
	}
	if current.Version != request.ExpectedVersion {
		return commerce.AdminProduct{}, commerce.ErrProductVersionConflict
	}

	name, description, priceAmount, status := current.Name, current.Description, current.PriceAmount, current.Status
	if request.Name != nil {
		name = *request.Name
	}
	if request.Description != nil {
		description = *request.Description
	}
	if request.PriceAmount != nil {
		priceAmount = *request.PriceAmount
	}
	if request.Status != nil {
		status = string(*request.Status)
	}
	now, err := databaseTime(ctx, tx)
	if err != nil {
		return commerce.AdminProduct{}, mapCommerceDatabaseError("read product update time", err)
	}
	updated, err := queries.UpdateProduct(ctx, store.UpdateProductParams{
		ID: productID, Name: name, Description: description, PriceAmount: priceAmount,
		Status: status, UpdatedAt: now, Version: request.ExpectedVersion,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return commerce.AdminProduct{}, commerce.ErrProductVersionConflict
	}
	if err != nil {
		return commerce.AdminProduct{}, mapCommerceDatabaseError("update product", err)
	}
	detail, err := queries.GetProductByID(ctx, productID)
	if err != nil {
		return commerce.AdminProduct{}, mapCommerceDatabaseError("read updated product", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return commerce.AdminProduct{}, mapCommerceDatabaseError("commit product update", err)
	}
	return adminProductFromDetail(updated, detail.Quantity, detail.InventoryVersion, detail.InventoryUpdatedAt), nil
}

func (r *OrderRepository) GetProduct(ctx context.Context, productID uuid.UUID) (commerce.Product, error) {
	row, err := r.queries.GetActiveProductByID(ctx, productID)
	if errors.Is(err, pgx.ErrNoRows) {
		return commerce.Product{}, commerce.ErrProductNotFound
	}
	if err != nil {
		return commerce.Product{}, mapCommerceDatabaseError("get product", err)
	}
	return productFromValues(
		row.ID, row.Sku, row.Name, row.Description, row.PriceAmount, row.Currency,
		row.Status, row.Version, row.Quantity, row.CreatedAt, row.UpdatedAt,
	), nil
}

func (r *OrderRepository) GetAdminProduct(
	ctx context.Context,
	actorID, productID uuid.UUID,
) (commerce.AdminProduct, error) {
	tx, queries, err := r.begin(ctx, "admin product read")
	if err != nil {
		return commerce.AdminProduct{}, err
	}
	defer rollbackCommerceTransaction(tx)
	if err := authorizeCommerceActor(ctx, queries, actorID, commerce.RoleAdmin); err != nil {
		return commerce.AdminProduct{}, err
	}
	row, err := queries.GetProductByID(ctx, productID)
	if errors.Is(err, pgx.ErrNoRows) {
		return commerce.AdminProduct{}, commerce.ErrProductNotFound
	}
	if err != nil {
		return commerce.AdminProduct{}, mapCommerceDatabaseError("get admin product", err)
	}
	result := commerce.AdminProduct{
		Product: productFromValues(
			row.ID, row.Sku, row.Name, row.Description, row.PriceAmount, row.Currency,
			row.Status, row.Version, row.Quantity, row.CreatedAt, row.UpdatedAt,
		),
		Inventory: inventoryFromValues(
			row.ID, row.Sku, row.Name, row.Status, row.Quantity, row.InventoryVersion, row.InventoryUpdatedAt,
		),
	}
	if err := tx.Commit(ctx); err != nil {
		return commerce.AdminProduct{}, mapCommerceDatabaseError("commit admin product read", err)
	}
	return result, nil
}

func (r *OrderRepository) ListProducts(ctx context.Context, page commerce.PageRequest) (commerce.ProductPage, error) {
	limit := page.Limit + 1
	var products []commerce.Product
	if page.Cursor == nil {
		rows, err := r.queries.ListActiveProducts(ctx, limit)
		if err != nil {
			return commerce.ProductPage{}, mapCommerceDatabaseError("list products", err)
		}
		products = make([]commerce.Product, 0, len(rows))
		for _, row := range rows {
			products = append(products, productFromValues(
				row.ID, row.Sku, row.Name, row.Description, row.PriceAmount, row.Currency,
				row.Status, row.Version, row.Quantity, row.CreatedAt, row.UpdatedAt,
			))
		}
	} else {
		rows, err := r.queries.ListActiveProductsAfter(ctx, store.ListActiveProductsAfterParams{
			CursorCreatedAt: page.Cursor.CreatedAt, CursorID: page.Cursor.ID, ResultLimit: limit,
		})
		if err != nil {
			return commerce.ProductPage{}, mapCommerceDatabaseError("list products after cursor", err)
		}
		products = make([]commerce.Product, 0, len(rows))
		for _, row := range rows {
			products = append(products, productFromValues(
				row.ID, row.Sku, row.Name, row.Description, row.PriceAmount, row.Currency,
				row.Status, row.Version, row.Quantity, row.CreatedAt, row.UpdatedAt,
			))
		}
	}
	return productPage(products, page.Limit), nil
}

func (r *OrderRepository) AdjustInventory(
	ctx context.Context,
	actorID, productID uuid.UUID,
	request commerce.AdjustInventoryRequest,
) (commerce.Inventory, error) {
	tx, queries, err := r.begin(ctx, "inventory adjustment")
	if err != nil {
		return commerce.Inventory{}, err
	}
	defer rollbackCommerceTransaction(tx)

	if err := authorizeCommerceActor(ctx, queries, actorID, commerce.RoleAdmin); err != nil {
		return commerce.Inventory{}, err
	}
	current, err := queries.GetInventoryForUpdate(ctx, productID)
	if errors.Is(err, pgx.ErrNoRows) {
		return commerce.Inventory{}, commerce.ErrProductNotFound
	}
	if err != nil {
		return commerce.Inventory{}, mapCommerceDatabaseError("lock inventory", err)
	}
	if current.Version != request.ExpectedVersion {
		return commerce.Inventory{}, commerce.ErrInventoryVersionConflict
	}
	if (request.Delta > 0 && current.Quantity > commerce.MaxInventoryQuantity-request.Delta) ||
		(request.Delta < 0 && current.Quantity < -request.Delta) {
		return commerce.Inventory{}, commerce.ErrInventoryOutOfRange
	}
	now, err := databaseTime(ctx, tx)
	if err != nil {
		return commerce.Inventory{}, mapCommerceDatabaseError("read inventory adjustment time", err)
	}
	updated, err := queries.UpdateInventory(ctx, store.UpdateInventoryParams{
		ProductID: productID, Quantity: current.Quantity + request.Delta,
		UpdatedAt: now, Version: request.ExpectedVersion,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return commerce.Inventory{}, commerce.ErrInventoryVersionConflict
	}
	if err != nil {
		return commerce.Inventory{}, mapCommerceDatabaseError("update inventory", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return commerce.Inventory{}, mapCommerceDatabaseError("commit inventory adjustment", err)
	}
	return inventoryFromValues(
		updated.ProductID, current.Sku, current.Name, current.Status,
		updated.Quantity, updated.Version, updated.UpdatedAt,
	), nil
}

func (r *OrderRepository) ListInventory(
	ctx context.Context,
	actorID uuid.UUID,
	page commerce.PageRequest,
) (commerce.InventoryPage, error) {
	tx, queries, err := r.begin(ctx, "inventory list")
	if err != nil {
		return commerce.InventoryPage{}, err
	}
	defer rollbackCommerceTransaction(tx)
	if err := authorizeCommerceActor(ctx, queries, actorID, commerce.RoleAdmin); err != nil {
		return commerce.InventoryPage{}, err
	}
	limit := page.Limit + 1
	items := make([]commerce.Inventory, 0, limit)
	cursors := make([]commerce.PageCursor, 0, limit)
	if page.Cursor == nil {
		rows, err := queries.ListInventory(ctx, limit)
		if err != nil {
			return commerce.InventoryPage{}, mapCommerceDatabaseError("list inventory", err)
		}
		for _, row := range rows {
			items = append(items, inventoryFromValues(
				row.ProductID, row.Sku, row.Name, row.Status, row.Quantity, row.Version, row.UpdatedAt,
			))
			cursors = append(cursors, commerce.PageCursor{CreatedAt: row.ProductCreatedAt, ID: row.ProductID})
		}
	} else {
		rows, err := queries.ListInventoryAfter(ctx, store.ListInventoryAfterParams{
			CursorCreatedAt: page.Cursor.CreatedAt, CursorID: page.Cursor.ID, ResultLimit: limit,
		})
		if err != nil {
			return commerce.InventoryPage{}, mapCommerceDatabaseError("list inventory after cursor", err)
		}
		for _, row := range rows {
			items = append(items, inventoryFromValues(
				row.ProductID, row.Sku, row.Name, row.Status, row.Quantity, row.Version, row.UpdatedAt,
			))
			cursors = append(cursors, commerce.PageCursor{CreatedAt: row.ProductCreatedAt, ID: row.ProductID})
		}
	}
	pageResult := commerce.InventoryPage{Inventories: items}
	if len(items) > int(page.Limit) {
		pageResult.Inventories = items[:page.Limit]
		cursor := cursors[page.Limit-1]
		pageResult.NextCursor = &cursor
	}
	if err := tx.Commit(ctx); err != nil {
		return commerce.InventoryPage{}, mapCommerceDatabaseError("commit inventory list", err)
	}
	return pageResult, nil
}

func (r *OrderRepository) CreateOrder(
	ctx context.Context,
	actorID uuid.UUID,
	params commerce.CreateOrderParams,
) (commerce.Order, error) {
	tx, queries, err := r.begin(ctx, "order creation")
	if err != nil {
		return commerce.Order{}, err
	}
	defer rollbackCommerceTransaction(tx)

	actor, err := queries.GetCommerceActorForShare(ctx, actorID)
	if errors.Is(err, pgx.ErrNoRows) {
		return commerce.Order{}, commerce.ErrForbidden
	}
	if err != nil {
		return commerce.Order{}, mapCommerceDatabaseError("lock customer", err)
	}
	if err := validateStoredActor(actor.Role, actor.Status, commerce.RoleCustomer); err != nil {
		return commerce.Order{}, err
	}

	items := make([]commerce.OrderItem, 0, len(params.Items))
	var totalAmount int64
	for _, requested := range params.Items {
		product, err := queries.GetProductForOrder(ctx, requested.ProductID)
		if errors.Is(err, pgx.ErrNoRows) {
			return commerce.Order{}, commerce.ErrProductUnavailable
		}
		if err != nil {
			return commerce.Order{}, mapCommerceDatabaseError("lock order product", err)
		}
		if product.Status != string(commerce.ProductStatusActive) {
			return commerce.Order{}, commerce.ErrProductUnavailable
		}
		if product.Version != requested.ExpectedProductVersion {
			return commerce.Order{}, commerce.ErrProductChanged
		}
		if product.Currency != actor.Currency {
			return commerce.Order{}, commerce.ErrCurrencyMismatch
		}
		if product.PriceAmount > math.MaxInt64/requested.Quantity {
			return commerce.Order{}, commerce.ErrOrderAmountTooLarge
		}
		lineAmount := product.PriceAmount * requested.Quantity
		if totalAmount > math.MaxInt64-lineAmount {
			return commerce.Order{}, commerce.ErrOrderAmountTooLarge
		}
		totalAmount += lineAmount
		items = append(items, commerce.OrderItem{
			ID: requested.ItemID, OrderID: params.OrderID, ProductID: product.ID,
			ProductSKU: product.Sku, ProductName: product.Name, ProductVersion: product.Version,
			Quantity: requested.Quantity, UnitPriceAmount: product.PriceAmount, LineAmount: lineAmount,
		})
	}

	now, err := databaseTime(ctx, tx)
	if err != nil {
		return commerce.Order{}, mapCommerceDatabaseError("read order creation time", err)
	}
	for _, requested := range params.Items {
		_, err := queries.DeductInventoryForOrder(ctx, store.DeductInventoryForOrderParams{
			ProductID: requested.ProductID, Quantity: requested.Quantity, UpdatedAt: now,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return commerce.Order{}, commerce.ErrInsufficientInventory
		}
		if err != nil {
			return commerce.Order{}, mapCommerceDatabaseError("deduct inventory", err)
		}
	}

	dbOrder, err := queries.CreateOrder(ctx, store.CreateOrderParams{
		ID: params.OrderID, UserID: actorID, Currency: actor.Currency,
		TotalAmount: totalAmount, CreatedAt: now,
	})
	if err != nil {
		return commerce.Order{}, mapCommerceDatabaseError("create order", err)
	}
	for index := range items {
		items[index].CreatedAt = now
		if _, err := queries.CreateOrderItem(ctx, store.CreateOrderItemParams{
			ID: items[index].ID, OrderID: params.OrderID, ProductID: items[index].ProductID,
			ProductSku: items[index].ProductSKU, ProductName: items[index].ProductName,
			ProductVersion: items[index].ProductVersion, Quantity: items[index].Quantity,
			UnitPriceAmount: items[index].UnitPriceAmount, LineAmount: items[index].LineAmount, CreatedAt: now,
		}); err != nil {
			return commerce.Order{}, mapCommerceDatabaseError("create order item", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return commerce.Order{}, mapCommerceDatabaseError("commit order creation", err)
	}
	result := orderFromModel(dbOrder)
	result.Items = items
	return result, nil
}

func (r *OrderRepository) ListOrders(
	ctx context.Context,
	actor commerce.Actor,
	page commerce.PageRequest,
) (commerce.OrderPage, error) {
	tx, queries, err := r.begin(ctx, "order list")
	if err != nil {
		return commerce.OrderPage{}, err
	}
	defer rollbackCommerceTransaction(tx)
	if err := authorizeCommerceActor(ctx, queries, actor.UserID, actor.Role); err != nil {
		return commerce.OrderPage{}, err
	}
	limit := page.Limit + 1
	var rows []store.Order
	var queryErr error
	if actor.Role == commerce.RoleAdmin {
		if page.Cursor == nil {
			rows, queryErr = queries.ListAllOrders(ctx, limit)
		} else {
			rows, queryErr = queries.ListAllOrdersAfter(ctx, store.ListAllOrdersAfterParams{
				CursorCreatedAt: page.Cursor.CreatedAt, CursorID: page.Cursor.ID, ResultLimit: limit,
			})
		}
	} else {
		if page.Cursor == nil {
			rows, queryErr = queries.ListOrdersByUser(ctx, store.ListOrdersByUserParams{UserID: actor.UserID, Limit: limit})
		} else {
			rows, queryErr = queries.ListOrdersByUserAfter(ctx, store.ListOrdersByUserAfterParams{
				UserID: actor.UserID, CursorCreatedAt: page.Cursor.CreatedAt,
				CursorID: page.Cursor.ID, ResultLimit: limit,
			})
		}
	}
	if queryErr != nil {
		return commerce.OrderPage{}, mapCommerceDatabaseError("list orders", queryErr)
	}
	result, err := r.orderPage(ctx, queries, rows, page.Limit)
	if err != nil {
		return commerce.OrderPage{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return commerce.OrderPage{}, mapCommerceDatabaseError("commit order list", err)
	}
	return result, nil
}

func (r *OrderRepository) GetOrder(
	ctx context.Context,
	actor commerce.Actor,
	orderID uuid.UUID,
) (commerce.Order, error) {
	tx, queries, err := r.begin(ctx, "order read")
	if err != nil {
		return commerce.Order{}, err
	}
	defer rollbackCommerceTransaction(tx)
	if err := authorizeCommerceActor(ctx, queries, actor.UserID, actor.Role); err != nil {
		return commerce.Order{}, err
	}
	var row store.Order
	var queryErr error
	if actor.Role == commerce.RoleAdmin {
		row, queryErr = queries.GetOrderByID(ctx, orderID)
	} else {
		row, queryErr = queries.GetOrderByIDAndUser(ctx, store.GetOrderByIDAndUserParams{ID: orderID, UserID: actor.UserID})
	}
	if errors.Is(queryErr, pgx.ErrNoRows) {
		return commerce.Order{}, commerce.ErrOrderNotFound
	}
	if queryErr != nil {
		return commerce.Order{}, mapCommerceDatabaseError("get order", queryErr)
	}
	items, err := queries.ListOrderItemsByOrderID(ctx, row.ID)
	if err != nil {
		return commerce.Order{}, mapCommerceDatabaseError("get order items", err)
	}
	result := orderFromModel(row)
	result.Items = orderItemsFromModels(items)
	if err := tx.Commit(ctx); err != nil {
		return commerce.Order{}, mapCommerceDatabaseError("commit order read", err)
	}
	return result, nil
}

func (r *OrderRepository) orderPage(
	ctx context.Context,
	queries *store.Queries,
	rows []store.Order,
	limit int32,
) (commerce.OrderPage, error) {
	result := commerce.OrderPage{Orders: make([]commerce.Order, 0, min(len(rows), int(limit)))}
	if len(rows) > int(limit) {
		cursor := commerce.PageCursor{CreatedAt: rows[limit-1].CreatedAt, ID: rows[limit-1].ID}
		result.NextCursor = &cursor
		rows = rows[:limit]
	}
	if len(rows) == 0 {
		return result, nil
	}
	orderIDs := make([]uuid.UUID, len(rows))
	for index, row := range rows {
		orderIDs[index] = row.ID
		result.Orders = append(result.Orders, orderFromModel(row))
	}
	dbItems, err := queries.ListOrderItemsByOrderIDs(ctx, orderIDs)
	if err != nil {
		return commerce.OrderPage{}, mapCommerceDatabaseError("list order items", err)
	}
	ordersByID := make(map[uuid.UUID]int, len(result.Orders))
	for index, current := range result.Orders {
		ordersByID[current.ID] = index
	}
	for _, item := range dbItems {
		index, ok := ordersByID[item.OrderID]
		if !ok {
			return commerce.OrderPage{}, fmt.Errorf("list order items: unknown order %s", item.OrderID)
		}
		result.Orders[index].Items = append(result.Orders[index].Items, orderItemFromModel(item))
	}
	return result, nil
}

func (r *OrderRepository) begin(ctx context.Context, operation string) (pgx.Tx, *store.Queries, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, nil, mapCommerceDatabaseError("begin "+operation, err)
	}
	if _, err := tx.Exec(
		ctx,
		`SELECT set_config('lock_timeout', $1, true), set_config('statement_timeout', $2, true)`,
		postgresDuration(r.lockTimeout),
		postgresDuration(r.statementTimeout),
	); err != nil {
		rollbackCommerceTransaction(tx)
		return nil, nil, mapCommerceDatabaseError("configure "+operation+" transaction", err)
	}
	return tx, r.queries.WithTx(tx), nil
}

func authorizeCommerceActor(
	ctx context.Context,
	queries *store.Queries,
	actorID uuid.UUID,
	requiredRole string,
) error {
	row, err := queries.GetCommerceActorForShare(ctx, actorID)
	if errors.Is(err, pgx.ErrNoRows) {
		return commerce.ErrForbidden
	}
	if err != nil {
		return mapCommerceDatabaseError("authorize actor", err)
	}
	return validateStoredActor(row.Role, row.Status, requiredRole)
}

func validateStoredActor(role, status, requiredRole string) error {
	if status != "active" {
		return commerce.ErrAccountDisabled
	}
	if role != requiredRole {
		return commerce.ErrForbidden
	}
	return nil
}

func rollbackCommerceTransaction(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}

func mapProductWriteError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "products_sku_unique" {
		return commerce.ErrSKUAlreadyExists
	}
	return mapCommerceDatabaseError("write product", err)
}

func mapCommerceDatabaseError(operation string, err error) error {
	if isDatabaseTimeout(err) {
		return commerce.ErrTemporarilyUnavailable
	}
	return fmt.Errorf("%s: %w", operation, err)
}

func productFromValues(
	id uuid.UUID,
	sku, name, description string,
	priceAmount int64,
	currency, status string,
	version, quantity int64,
	createdAt, updatedAt time.Time,
) commerce.Product {
	return commerce.Product{
		ID: id, SKU: sku, Name: name, Description: description, PriceAmount: priceAmount,
		Currency: currency, Status: commerce.ProductStatus(status), Version: version,
		Available: quantity > 0, CreatedAt: createdAt, UpdatedAt: updatedAt,
	}
}

func inventoryFromValues(
	productID uuid.UUID,
	sku, name, status string,
	quantity, version int64,
	updatedAt time.Time,
) commerce.Inventory {
	return commerce.Inventory{
		ProductID: productID, SKU: sku, Name: name, Status: commerce.ProductStatus(status),
		Quantity: quantity, Version: version, UpdatedAt: updatedAt,
	}
}

func adminProductFromModels(product store.Product, inventory store.Inventory) commerce.AdminProduct {
	return adminProductFromDetail(product, inventory.Quantity, inventory.Version, inventory.UpdatedAt)
}

func adminProductFromDetail(
	product store.Product,
	quantity, inventoryVersion int64,
	inventoryUpdatedAt time.Time,
) commerce.AdminProduct {
	return commerce.AdminProduct{
		Product: productFromValues(
			product.ID, product.Sku, product.Name, product.Description, product.PriceAmount,
			product.Currency, product.Status, product.Version, quantity, product.CreatedAt, product.UpdatedAt,
		),
		Inventory: inventoryFromValues(
			product.ID, product.Sku, product.Name, product.Status, quantity, inventoryVersion, inventoryUpdatedAt,
		),
	}
}

func productPage(products []commerce.Product, limit int32) commerce.ProductPage {
	result := commerce.ProductPage{Products: products}
	if len(products) > int(limit) {
		result.Products = products[:limit]
		last := result.Products[len(result.Products)-1]
		cursor := commerce.PageCursor{CreatedAt: last.CreatedAt, ID: last.ID}
		result.NextCursor = &cursor
	}
	return result
}

func orderFromModel(model store.Order) commerce.Order {
	return commerce.Order{
		ID: model.ID, UserID: model.UserID, Status: commerce.OrderStatus(model.Status),
		Currency: model.Currency, TotalAmount: model.TotalAmount,
		Items: []commerce.OrderItem{}, CreatedAt: model.CreatedAt, UpdatedAt: model.UpdatedAt,
	}
}

func orderItemFromModel(model store.OrderItem) commerce.OrderItem {
	return commerce.OrderItem{
		ID: model.ID, OrderID: model.OrderID, ProductID: model.ProductID,
		ProductSKU: model.ProductSku, ProductName: model.ProductName, ProductVersion: model.ProductVersion,
		Quantity: model.Quantity, UnitPriceAmount: model.UnitPriceAmount,
		LineAmount: model.LineAmount, CreatedAt: model.CreatedAt,
	}
}

func orderItemsFromModels(models []store.OrderItem) []commerce.OrderItem {
	items := make([]commerce.OrderItem, 0, len(models))
	for _, model := range models {
		items = append(items, orderItemFromModel(model))
	}
	return items
}
