package order

import (
	"fmt"
	"testing"

	"github.com/google/uuid"
)

var benchmarkOrderHash [32]byte

func BenchmarkOrderRequestHash(b *testing.B) {
	for _, size := range []int{1, 10, 50} {
		items := make([]RequestedItem, size)
		for index := range items {
			items[index] = RequestedItem{
				ProductID: uuid.MustParse(fmt.Sprintf("00000000-0000-4000-8000-%012d", index+1)),
				Quantity:  int64(index%10 + 1), ExpectedProductVersion: int64(index + 1),
			}
		}
		b.Run(fmt.Sprintf("items_%d", size), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				benchmarkOrderHash = orderRequestHash(items)
			}
		})
	}
}
