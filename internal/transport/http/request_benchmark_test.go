package httptransport

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	commerce "github.com/assumeengagetry/distributed-commerce/internal/order"
)

var benchmarkCursor commerce.PageCursor

func BenchmarkValidateJSONKeys(b *testing.B) {
	fixtures := map[string]any{
		"small":          map[string]any{"email": "user@example.com", "password": "benchmark-password"},
		"order_50_items": benchmarkOrderPayload(50),
	}
	for name, fixture := range fixtures {
		encoded, err := json.Marshal(fixture)
		if err != nil {
			b.Fatalf("json.Marshal(%s) error = %v", name, err)
		}
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if err := validateJSONKeys(encoded); err != nil {
					b.Fatalf("validateJSONKeys() error = %v", err)
				}
			}
		})
	}
}

func BenchmarkDecodeCursor(b *testing.B) {
	encoded, err := encodeCursor(&commerce.PageCursor{
		CreatedAt: time.Date(2026, time.August, 17, 12, 0, 0, 123, time.UTC),
		ID:        uuid.MustParse("11111111-1111-4111-8111-111111111111"),
	})
	if err != nil || encoded == nil {
		b.Fatalf("encodeCursor() = (%v, %v)", encoded, err)
	}
	b.ReportAllocs()
	for b.Loop() {
		benchmarkCursor, err = decodeCursor(*encoded)
		if err != nil {
			b.Fatalf("decodeCursor() error = %v", err)
		}
	}
}

func benchmarkOrderPayload(size int) map[string]any {
	items := make([]map[string]any, size)
	for index := range items {
		items[index] = map[string]any{
			"product_id":               uuid.NewSHA1(uuid.Nil, []byte{byte(index)}).String(),
			"quantity":                 index%10 + 1,
			"expected_product_version": index + 1,
		}
	}
	return map[string]any{"items": items}
}
