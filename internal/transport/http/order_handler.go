package httptransport

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	commerce "github.com/assumeengagetry/distributed-commerce/internal/order"
)

type createOrderBody struct {
	Items []createOrderItemBody `json:"items"`
}

type createOrderItemBody struct {
	ProductID              uuid.UUID `json:"product_id"`
	Quantity               int64     `json:"quantity"`
	ExpectedProductVersion int64     `json:"expected_product_version"`
}

type orderItemResponse struct {
	ID              uuid.UUID `json:"id"`
	ProductID       uuid.UUID `json:"product_id"`
	SKU             string    `json:"sku"`
	Name            string    `json:"name"`
	ProductVersion  int64     `json:"product_version"`
	Quantity        int64     `json:"quantity"`
	UnitPriceAmount int64     `json:"unit_price_amount"`
	LineAmount      int64     `json:"line_amount"`
}

type orderResponse struct {
	ID          uuid.UUID            `json:"id"`
	UserID      uuid.UUID            `json:"user_id"`
	Status      commerce.OrderStatus `json:"status"`
	Currency    string               `json:"currency"`
	TotalAmount int64                `json:"total_amount"`
	Items       []orderItemResponse  `json:"items"`
	CreatedAt   time.Time            `json:"created_at"`
	UpdatedAt   time.Time            `json:"updated_at"`
}

func (h commerceHandler) createOrder(c *gin.Context) {
	idempotencyKey, err := idempotencyKeyFromRequest(c)
	if err != nil {
		writeIdempotencyHeaderError(c, err)
		return
	}
	var body createOrderBody
	if err := decodeJSON(c, &body); err != nil {
		writeDecodeError(c, err)
		return
	}
	actor, ok := actorFromContext(c)
	if !ok {
		return
	}
	items := make([]commerce.RequestedItem, len(body.Items))
	for index, item := range body.Items {
		items[index] = commerce.RequestedItem{
			ProductID: item.ProductID, Quantity: item.Quantity,
			ExpectedProductVersion: item.ExpectedProductVersion,
		}
	}
	created, err := h.service.CreateOrder(c.Request.Context(), actor, commerce.CreateOrderRequest{
		Items: items, IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		h.writeServiceError(c, err)
		return
	}
	c.Header("Location", "/v1/orders/"+created.ID.String())
	if created.IdempotencyReplay {
		c.Header("Idempotency-Replayed", "true")
	}
	c.JSON(http.StatusCreated, gin.H{"order": orderResult(created)})
}

func (h commerceHandler) listOrders(c *gin.Context) {
	page, err := parsePage(c)
	if err != nil {
		writePaginationError(c, err)
		return
	}
	actor, ok := actorFromContext(c)
	if !ok {
		return
	}
	result, err := h.service.ListOrders(c.Request.Context(), actor, page)
	if err != nil {
		h.writeServiceError(c, err)
		return
	}
	nextCursor, err := encodeCursor(result.NextCursor)
	if err != nil {
		h.writeServiceError(c, err)
		return
	}
	orders := make([]orderResponse, 0, len(result.Orders))
	for _, current := range result.Orders {
		orders = append(orders, orderResult(current))
	}
	c.JSON(http.StatusOK, gin.H{
		"orders":     orders,
		"pagination": paginationResponse{Limit: page.Limit, NextCursor: nextCursor},
	})
}

func (h commerceHandler) getOrder(c *gin.Context) {
	orderID, ok := pathUUID(c, "order_id", "INVALID_ORDER_ID")
	if !ok {
		return
	}
	actor, ok := actorFromContext(c)
	if !ok {
		return
	}
	result, err := h.service.GetOrder(c.Request.Context(), actor, orderID)
	if err != nil {
		h.writeServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"order": orderResult(result)})
}

func orderResult(current commerce.Order) orderResponse {
	items := make([]orderItemResponse, 0, len(current.Items))
	for _, item := range current.Items {
		items = append(items, orderItemResponse{
			ID: item.ID, ProductID: item.ProductID, SKU: item.ProductSKU, Name: item.ProductName,
			ProductVersion: item.ProductVersion, Quantity: item.Quantity,
			UnitPriceAmount: item.UnitPriceAmount, LineAmount: item.LineAmount,
		})
	}
	return orderResponse{
		ID: current.ID, UserID: current.UserID, Status: current.Status, Currency: current.Currency,
		TotalAmount: current.TotalAmount, Items: items,
		CreatedAt: current.CreatedAt, UpdatedAt: current.UpdatedAt,
	}
}
