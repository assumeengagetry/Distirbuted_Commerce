package httptransport

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	commerce "github.com/assumeengagetry/distributed-commerce/internal/order"
)

const defaultPageSize int32 = 20
const maximumCursorSize = 512

var (
	errInvalidQuery  = errors.New("invalid query parameters")
	errInvalidCursor = errors.New("invalid pagination cursor")
)

type cursorPayload struct {
	Version   int    `json:"v"`
	CreatedAt string `json:"created_at"`
	ID        string `json:"id"`
}

func parsePage(c *gin.Context) (commerce.PageRequest, error) {
	query, err := url.ParseQuery(c.Request.URL.RawQuery)
	if err != nil {
		return commerce.PageRequest{}, errInvalidQuery
	}
	if err := validatePageQuery(query); err != nil {
		return commerce.PageRequest{}, err
	}
	page := commerce.PageRequest{Limit: defaultPageSize}
	if raw := query.Get("limit"); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 32)
		if err != nil || value < 1 || value > int64(commerce.MaxPageSize) {
			return commerce.PageRequest{}, errInvalidQuery
		}
		page.Limit = int32(value)
	}
	if raw := query.Get("cursor"); raw != "" {
		cursor, err := decodeCursor(raw)
		if err != nil {
			return commerce.PageRequest{}, err
		}
		page.Cursor = &cursor
	}
	return page, nil
}

func validatePageQuery(query url.Values) error {
	for key, values := range query {
		if key != "limit" && key != "cursor" {
			return errInvalidQuery
		}
		if len(values) != 1 || values[0] == "" {
			return errInvalidQuery
		}
	}
	return nil
}

func encodeCursor(cursor *commerce.PageCursor) (*string, error) {
	if cursor == nil {
		return nil, nil
	}
	payload, err := json.Marshal(cursorPayload{
		Version: 1, CreatedAt: cursor.CreatedAt.UTC().Format(time.RFC3339Nano), ID: cursor.ID.String(),
	})
	if err != nil {
		return nil, fmt.Errorf("marshal pagination cursor: %w", err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	return &encoded, nil
}

func decodeCursor(encoded string) (commerce.PageCursor, error) {
	if len(encoded) == 0 || len(encoded) > maximumCursorSize {
		return commerce.PageCursor{}, errInvalidCursor
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil || base64.RawURLEncoding.EncodeToString(decoded) != encoded {
		return commerce.PageCursor{}, errInvalidCursor
	}
	if err := validateJSONKeys(decoded); err != nil {
		return commerce.PageCursor{}, errInvalidCursor
	}
	decoder := json.NewDecoder(bytes.NewReader(decoded))
	decoder.DisallowUnknownFields()
	var payload cursorPayload
	if err := decoder.Decode(&payload); err != nil {
		return commerce.PageCursor{}, errInvalidCursor
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return commerce.PageCursor{}, errInvalidCursor
	}
	createdAt, err := time.Parse(time.RFC3339Nano, payload.CreatedAt)
	if err != nil || createdAt.Location() != time.UTC {
		return commerce.PageCursor{}, errInvalidCursor
	}
	id, err := uuid.Parse(payload.ID)
	if err != nil || id == uuid.Nil || id.String() != payload.ID || payload.Version != 1 {
		return commerce.PageCursor{}, errInvalidCursor
	}
	return commerce.PageCursor{CreatedAt: createdAt, ID: id}, nil
}
