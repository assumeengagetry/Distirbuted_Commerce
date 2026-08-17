package auth

import "testing"

var benchmarkRefreshToken RefreshToken

func BenchmarkParseRefreshToken(b *testing.B) {
	token, err := NewRefreshToken()
	if err != nil {
		b.Fatalf("NewRefreshToken() error = %v", err)
	}
	b.ReportAllocs()
	for b.Loop() {
		benchmarkRefreshToken, err = ParseRefreshToken(token.Raw)
		if err != nil {
			b.Fatalf("ParseRefreshToken() error = %v", err)
		}
	}
}
