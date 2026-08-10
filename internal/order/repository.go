package order

import (
	"context"

	"github.com/google/uuid"
)

type Repository interface {
	CreateProduct(ctx context.Context, actorID uuid.UUID, params CreateProductParams) (AdminProduct, error)
	UpdateProduct(ctx context.Context, actorID, productID uuid.UUID, request UpdateProductRequest) (AdminProduct, error)
	GetProduct(ctx context.Context, productID uuid.UUID) (Product, error)
	GetAdminProduct(ctx context.Context, actorID, productID uuid.UUID) (AdminProduct, error)
	ListProducts(ctx context.Context, page PageRequest) (ProductPage, error)
	AdjustInventory(ctx context.Context, actorID, productID uuid.UUID, request AdjustInventoryRequest) (Inventory, error)
	ListInventory(ctx context.Context, actorID uuid.UUID, page PageRequest) (InventoryPage, error)
	CreateOrder(ctx context.Context, actorID uuid.UUID, params CreateOrderParams) (Order, error)
	ListOrders(ctx context.Context, actor Actor, page PageRequest) (OrderPage, error)
	GetOrder(ctx context.Context, actor Actor, orderID uuid.UUID) (Order, error)
}
