package productcache

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/assumeengagetry/distributed-commerce/internal/order"
)

var (
	benchmarkCacheEntry   cacheEntry
	benchmarkCacheProduct order.Product
	benchmarkCacheValid   bool
)

func BenchmarkDecodeAndValidateCacheEntry(b *testing.B) {
	productID := uuid.MustParse("22222222-2222-4222-8222-222222222222")
	now := time.Date(2026, time.August, 17, 12, 0, 0, 123, time.UTC)
	product := order.Product{
		ID: productID, SKU: "BENCHMARK.CACHE", Name: "Benchmark product", Description: "cache payload",
		PriceAmount: 2500, Currency: order.CurrencyUSD, Status: order.ProductStatusActive,
		Version: 7, Available: true, CreatedAt: now, UpdatedAt: now,
	}
	encoded, err := json.Marshal(cacheEntry{
		SchemaVersion: cacheSchemaVersion, Generation: "33333333-3333-4333-8333-333333333333",
		Product: cacheProductFromProduct(product),
	})
	if err != nil {
		b.Fatalf("json.Marshal() error = %v", err)
	}
	encodedEntry := string(encoded)
	b.ReportAllocs()
	for b.Loop() {
		benchmarkCacheEntry, err = decodeCacheEntry(encodedEntry)
		if err != nil {
			b.Fatalf("decodeCacheEntry() error = %v", err)
		}
		benchmarkCacheProduct, benchmarkCacheValid = benchmarkCacheEntry.Product.toProduct(productID)
		if !benchmarkCacheValid {
			b.Fatal("decoded cache product is invalid")
		}
	}
}
