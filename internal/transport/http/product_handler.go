package httptransport

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/assumeengagetry/distributed-commerce/internal/auth"
	commerce "github.com/assumeengagetry/distributed-commerce/internal/order"
)

type commerceHandler struct {
	service CommerceService
	logger  *slog.Logger
}

type createProductBody struct {
	SKU             string                 `json:"sku"`
	Name            string                 `json:"name"`
	Description     string                 `json:"description"`
	PriceAmount     int64                  `json:"price_amount"`
	Currency        string                 `json:"currency"`
	Status          commerce.ProductStatus `json:"status"`
	InitialQuantity int64                  `json:"initial_quantity"`
}

type updateProductBody struct {
	ExpectedVersion int64                   `json:"expected_version"`
	Name            *string                 `json:"name"`
	Description     *string                 `json:"description"`
	PriceAmount     *int64                  `json:"price_amount"`
	Status          *commerce.ProductStatus `json:"status"`
}

type adjustInventoryBody struct {
	ExpectedVersion int64 `json:"expected_version"`
	Delta           int64 `json:"delta"`
}

type productResponse struct {
	ID           uuid.UUID `json:"id"`
	SKU          string    `json:"sku"`
	Name         string    `json:"name"`
	Description  string    `json:"description"`
	PriceAmount  int64     `json:"price_amount"`
	Currency     string    `json:"currency"`
	Version      int64     `json:"version"`
	Availability string    `json:"availability"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type inventoryResponse struct {
	ProductID uuid.UUID              `json:"product_id"`
	SKU       string                 `json:"sku"`
	Name      string                 `json:"name"`
	Status    commerce.ProductStatus `json:"status"`
	Quantity  int64                  `json:"quantity"`
	Version   int64                  `json:"version"`
	UpdatedAt time.Time              `json:"updated_at"`
}

type adminProductDetailResponse struct {
	productResponse
	Status commerce.ProductStatus `json:"status"`
}

type adminProductResponse struct {
	Product   adminProductDetailResponse `json:"product"`
	Inventory inventoryResponse          `json:"inventory"`
}

type paginationResponse struct {
	Limit      int32   `json:"limit"`
	NextCursor *string `json:"next_cursor"`
}

func (h commerceHandler) createProduct(c *gin.Context) {
	var body createProductBody
	if err := decodeJSON(c, &body); err != nil {
		writeDecodeError(c, err)
		return
	}
	actor, ok := actorFromContext(c)
	if !ok {
		return
	}
	created, err := h.service.CreateProduct(c.Request.Context(), actor, commerce.CreateProductRequest{
		SKU: body.SKU, Name: body.Name, Description: body.Description,
		PriceAmount: body.PriceAmount, Currency: body.Currency,
		Status: body.Status, InitialQuantity: body.InitialQuantity,
	})
	if err != nil {
		h.writeServiceError(c, err)
		return
	}
	c.Header("Location", "/v1/admin/products/"+created.Product.ID.String())
	c.JSON(http.StatusCreated, adminProductResult(created))
}

func (h commerceHandler) updateProduct(c *gin.Context) {
	productID, ok := pathUUID(c, "product_id", "INVALID_PRODUCT_ID")
	if !ok {
		return
	}
	var body updateProductBody
	if err := decodeJSON(c, &body); err != nil {
		writeDecodeError(c, err)
		return
	}
	actor, ok := actorFromContext(c)
	if !ok {
		return
	}
	updated, err := h.service.UpdateProduct(c.Request.Context(), actor, productID, commerce.UpdateProductRequest{
		ExpectedVersion: body.ExpectedVersion, Name: body.Name, Description: body.Description,
		PriceAmount: body.PriceAmount, Status: body.Status,
	})
	if err != nil {
		h.writeServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, adminProductResult(updated))
}

func (h commerceHandler) getProduct(c *gin.Context) {
	productID, ok := pathUUID(c, "product_id", "INVALID_PRODUCT_ID")
	if !ok {
		return
	}
	product, err := h.service.GetProduct(c.Request.Context(), productID)
	if err != nil {
		h.writeServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"product": productResult(product)})
}

func (h commerceHandler) getAdminProduct(c *gin.Context) {
	productID, ok := pathUUID(c, "product_id", "INVALID_PRODUCT_ID")
	if !ok {
		return
	}
	actor, ok := actorFromContext(c)
	if !ok {
		return
	}
	product, err := h.service.GetAdminProduct(c.Request.Context(), actor, productID)
	if err != nil {
		h.writeServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, adminProductResult(product))
}

func (h commerceHandler) listProducts(c *gin.Context) {
	page, err := parsePage(c)
	if err != nil {
		writePaginationError(c, err)
		return
	}
	result, err := h.service.ListProducts(c.Request.Context(), page)
	if err != nil {
		h.writeServiceError(c, err)
		return
	}
	nextCursor, err := encodeCursor(result.NextCursor)
	if err != nil {
		h.writeServiceError(c, err)
		return
	}
	products := make([]productResponse, 0, len(result.Products))
	for _, product := range result.Products {
		products = append(products, productResult(product))
	}
	c.JSON(http.StatusOK, gin.H{
		"products":   products,
		"pagination": paginationResponse{Limit: page.Limit, NextCursor: nextCursor},
	})
}

func (h commerceHandler) adjustInventory(c *gin.Context) {
	productID, ok := pathUUID(c, "product_id", "INVALID_PRODUCT_ID")
	if !ok {
		return
	}
	var body adjustInventoryBody
	if err := decodeJSON(c, &body); err != nil {
		writeDecodeError(c, err)
		return
	}
	actor, ok := actorFromContext(c)
	if !ok {
		return
	}
	inventory, err := h.service.AdjustInventory(c.Request.Context(), actor, productID, commerce.AdjustInventoryRequest{
		ExpectedVersion: body.ExpectedVersion, Delta: body.Delta,
	})
	if err != nil {
		h.writeServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"inventory": inventoryResult(inventory)})
}

func (h commerceHandler) listInventory(c *gin.Context) {
	page, err := parsePage(c)
	if err != nil {
		writePaginationError(c, err)
		return
	}
	actor, ok := actorFromContext(c)
	if !ok {
		return
	}
	result, err := h.service.ListInventory(c.Request.Context(), actor, page)
	if err != nil {
		h.writeServiceError(c, err)
		return
	}
	nextCursor, err := encodeCursor(result.NextCursor)
	if err != nil {
		h.writeServiceError(c, err)
		return
	}
	inventory := make([]inventoryResponse, 0, len(result.Inventories))
	for _, item := range result.Inventories {
		inventory = append(inventory, inventoryResult(item))
	}
	c.JSON(http.StatusOK, gin.H{
		"inventory":  inventory,
		"pagination": paginationResponse{Limit: page.Limit, NextCursor: nextCursor},
	})
}

func (h commerceHandler) writeServiceError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, commerce.ErrInvalidSKU):
		writeAPIError(c, http.StatusBadRequest, "INVALID_SKU", err.Error())
	case errors.Is(err, commerce.ErrInvalidProductName):
		writeAPIError(c, http.StatusBadRequest, "INVALID_PRODUCT_NAME", err.Error())
	case errors.Is(err, commerce.ErrInvalidDescription):
		writeAPIError(c, http.StatusBadRequest, "INVALID_DESCRIPTION", err.Error())
	case errors.Is(err, commerce.ErrInvalidPrice):
		writeAPIError(c, http.StatusBadRequest, "INVALID_PRICE", err.Error())
	case errors.Is(err, commerce.ErrInvalidCurrency):
		writeAPIError(c, http.StatusBadRequest, "INVALID_CURRENCY", err.Error())
	case errors.Is(err, commerce.ErrInvalidProductStatus):
		writeAPIError(c, http.StatusBadRequest, "INVALID_PRODUCT_STATUS", err.Error())
	case errors.Is(err, commerce.ErrInvalidInitialQuantity):
		writeAPIError(c, http.StatusBadRequest, "INVALID_INITIAL_QUANTITY", err.Error())
	case errors.Is(err, commerce.ErrEmptyProductUpdate):
		writeAPIError(c, http.StatusBadRequest, "EMPTY_PRODUCT_UPDATE", err.Error())
	case errors.Is(err, commerce.ErrInvalidExpectedVersion):
		writeAPIError(c, http.StatusBadRequest, "INVALID_EXPECTED_VERSION", err.Error())
	case errors.Is(err, commerce.ErrInvalidInventoryDelta):
		writeAPIError(c, http.StatusBadRequest, "INVALID_INVENTORY_DELTA", err.Error())
	case errors.Is(err, commerce.ErrInvalidOrderItems):
		writeAPIError(c, http.StatusBadRequest, "INVALID_ORDER_ITEMS", err.Error())
	case errors.Is(err, commerce.ErrInvalidItemQuantity):
		writeAPIError(c, http.StatusBadRequest, "INVALID_ITEM_QUANTITY", err.Error())
	case errors.Is(err, commerce.ErrInvalidPage):
		writeAPIError(c, http.StatusBadRequest, "INVALID_QUERY", "invalid pagination parameters")
	case errors.Is(err, commerce.ErrInvalidActor):
		c.Header("WWW-Authenticate", "Bearer")
		writeAPIError(c, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
	case errors.Is(err, commerce.ErrForbidden):
		writeAPIError(c, http.StatusForbidden, "FORBIDDEN", "permission denied")
	case errors.Is(err, commerce.ErrAccountDisabled):
		c.Header("WWW-Authenticate", "Bearer")
		writeAPIError(c, http.StatusUnauthorized, "ACCOUNT_DISABLED", "account is disabled")
	case errors.Is(err, commerce.ErrProductNotFound):
		writeAPIError(c, http.StatusNotFound, "PRODUCT_NOT_FOUND", "product not found")
	case errors.Is(err, commerce.ErrOrderNotFound):
		writeAPIError(c, http.StatusNotFound, "ORDER_NOT_FOUND", "order not found")
	case errors.Is(err, commerce.ErrSKUAlreadyExists):
		writeAPIError(c, http.StatusConflict, "SKU_ALREADY_EXISTS", "SKU already exists")
	case errors.Is(err, commerce.ErrProductVersionConflict):
		writeAPIError(c, http.StatusConflict, "PRODUCT_VERSION_CONFLICT", "product was modified")
	case errors.Is(err, commerce.ErrInventoryVersionConflict):
		writeAPIError(c, http.StatusConflict, "INVENTORY_VERSION_CONFLICT", "inventory was modified")
	case errors.Is(err, commerce.ErrInventoryOutOfRange):
		writeAPIError(c, http.StatusConflict, "INVENTORY_OUT_OF_RANGE", err.Error())
	case errors.Is(err, commerce.ErrProductUnavailable):
		writeAPIError(c, http.StatusConflict, "PRODUCT_UNAVAILABLE", "product is unavailable")
	case errors.Is(err, commerce.ErrProductChanged):
		writeAPIError(c, http.StatusConflict, "PRODUCT_CHANGED", "product changed; refresh the catalog")
	case errors.Is(err, commerce.ErrInsufficientInventory):
		writeAPIError(c, http.StatusConflict, "INSUFFICIENT_INVENTORY", "insufficient inventory")
	case errors.Is(err, commerce.ErrCurrencyMismatch):
		writeAPIError(c, http.StatusConflict, "CURRENCY_MISMATCH", "product currency does not match account")
	case errors.Is(err, commerce.ErrOrderAmountTooLarge):
		writeAPIError(c, http.StatusUnprocessableEntity, "ORDER_AMOUNT_TOO_LARGE", err.Error())
	case errors.Is(err, commerce.ErrTemporarilyUnavailable):
		c.Header("Retry-After", "1")
		writeAPIError(c, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "service temporarily unavailable")
	default:
		h.logger.ErrorContext(
			c.Request.Context(), "commerce request failed",
			slog.String("request_id", requestIDFromContext(c)), slog.Any("error", err),
		)
		writeAPIError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "internal server error")
	}
}

func actorFromContext(c *gin.Context) (commerce.Actor, bool) {
	principal, ok := auth.PrincipalFromContext(c.Request.Context())
	if !ok {
		writeAPIError(c, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
		return commerce.Actor{}, false
	}
	return commerce.Actor{UserID: principal.UserID, Role: principal.Role}, true
}

func pathUUID(c *gin.Context, name, code string) (uuid.UUID, bool) {
	value, err := uuid.Parse(c.Param(name))
	if err != nil || value == uuid.Nil || value.String() != c.Param(name) {
		writeAPIError(c, http.StatusBadRequest, code, "invalid resource ID")
		return uuid.Nil, false
	}
	return value, true
}

func writePaginationError(c *gin.Context, err error) {
	if errors.Is(err, errInvalidCursor) {
		writeAPIError(c, http.StatusBadRequest, "INVALID_CURSOR", "invalid pagination cursor")
		return
	}
	writeAPIError(c, http.StatusBadRequest, "INVALID_QUERY", "invalid query parameters")
}

func productResult(product commerce.Product) productResponse {
	availability := "out_of_stock"
	if product.Available {
		availability = "in_stock"
	}
	return productResponse{
		ID: product.ID, SKU: product.SKU, Name: product.Name, Description: product.Description,
		PriceAmount: product.PriceAmount, Currency: product.Currency, Version: product.Version,
		Availability: availability, CreatedAt: product.CreatedAt, UpdatedAt: product.UpdatedAt,
	}
}

func inventoryResult(inventory commerce.Inventory) inventoryResponse {
	return inventoryResponse{
		ProductID: inventory.ProductID, SKU: inventory.SKU, Name: inventory.Name,
		Status: inventory.Status, Quantity: inventory.Quantity,
		Version: inventory.Version, UpdatedAt: inventory.UpdatedAt,
	}
}

func adminProductResult(product commerce.AdminProduct) adminProductResponse {
	return adminProductResponse{
		Product: adminProductDetailResponse{
			productResponse: productResult(product.Product), Status: product.Product.Status,
		},
		Inventory: inventoryResult(product.Inventory),
	}
}
