package order

import (
	"time"

	"github.com/google/uuid"
)

const (
	RoleCustomer = "customer"
	RoleAdmin    = "admin"
	CurrencyUSD  = "USD"

	MaxPriceAmount       int64 = 1_000_000_000_000
	MaxInventoryQuantity int64 = 1_000_000_000
	MaxOrderItems              = 50
	MaxItemQuantity      int64 = 1_000
	MaxPageSize          int32 = 100
)

type ProductStatus string

const (
	ProductStatusActive   ProductStatus = "active"
	ProductStatusInactive ProductStatus = "inactive"
)

func (s ProductStatus) Valid() bool {
	return s == ProductStatusActive || s == ProductStatusInactive
}

type OrderStatus string

const (
	OrderStatusPending       OrderStatus = "pending"
	OrderStatusPaid          OrderStatus = "paid"
	OrderStatusPaymentFailed OrderStatus = "payment_failed"
	OrderStatusCancelled     OrderStatus = "cancelled"
)

type Actor struct {
	UserID uuid.UUID
	Role   string
}

type PageCursor struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

type PageRequest struct {
	Limit  int32
	Cursor *PageCursor
}

type Product struct {
	ID          uuid.UUID
	SKU         string
	Name        string
	Description string
	PriceAmount int64
	Currency    string
	Status      ProductStatus
	Version     int64
	Available   bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type Inventory struct {
	ProductID uuid.UUID
	SKU       string
	Name      string
	Status    ProductStatus
	Quantity  int64
	Version   int64
	UpdatedAt time.Time
}

type AdminProduct struct {
	Product   Product
	Inventory Inventory
}

type ProductPage struct {
	Products   []Product
	NextCursor *PageCursor
}

type InventoryPage struct {
	Inventories []Inventory
	NextCursor  *PageCursor
}

type OrderItem struct {
	ID              uuid.UUID
	OrderID         uuid.UUID
	ProductID       uuid.UUID
	ProductSKU      string
	ProductName     string
	ProductVersion  int64
	Quantity        int64
	UnitPriceAmount int64
	LineAmount      int64
	CreatedAt       time.Time
}

type Order struct {
	ID                uuid.UUID
	UserID            uuid.UUID
	Status            OrderStatus
	Currency          string
	TotalAmount       int64
	Items             []OrderItem
	CreatedAt         time.Time
	UpdatedAt         time.Time
	IdempotencyReplay bool
}

type OrderPage struct {
	Orders     []Order
	NextCursor *PageCursor
}

type CreateProductRequest struct {
	SKU             string
	Name            string
	Description     string
	PriceAmount     int64
	Currency        string
	Status          ProductStatus
	InitialQuantity int64
}

type CreateProductParams struct {
	ProductID uuid.UUID
	Request   CreateProductRequest
}

type UpdateProductRequest struct {
	ExpectedVersion int64
	Name            *string
	Description     *string
	PriceAmount     *int64
	Status          *ProductStatus
}

type AdjustInventoryRequest struct {
	ExpectedVersion int64
	Delta           int64
}

type RequestedItem struct {
	ItemID                 uuid.UUID
	ProductID              uuid.UUID
	Quantity               int64
	ExpectedProductVersion int64
}

type CreateOrderRequest struct {
	Items          []RequestedItem
	IdempotencyKey string
}

type CreateOrderParams struct {
	OrderID     uuid.UUID
	Items       []RequestedItem
	KeyHash     []byte
	RequestHash []byte
}
