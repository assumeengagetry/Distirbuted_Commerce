package idempotency

import (
	"bytes"
	"testing"
)

func TestValidKey(t *testing.T) {
	t.Parallel()
	tests := []struct {
		key  string
		want bool
	}{
		{key: "order-123", want: true},
		{key: "A.b_c:d-1", want: true},
		{key: "", want: false},
		{key: "-starts-with-symbol", want: false},
		{key: "contains space", want: false},
		{key: "contains,comma", want: false},
		{key: string(bytes.Repeat([]byte{'a'}, MaximumKeyLength)), want: true},
		{key: string(bytes.Repeat([]byte{'a'}, MaximumKeyLength+1)), want: false},
	}
	for _, test := range tests {
		if got := ValidKey(test.key); got != test.want {
			t.Errorf("ValidKey(%q) = %t, want %t", test.key, got, test.want)
		}
	}
}

func TestRequestHashSeparatesOperationsAndPayloads(t *testing.T) {
	t.Parallel()
	first := RequestHash(OrderCreateOperation, []byte("payload"))
	same := RequestHash(OrderCreateOperation, []byte("payload"))
	otherOperation := RequestHash(PaymentCreateOperation, []byte("payload"))
	otherPayload := RequestHash(OrderCreateOperation, []byte("other"))
	if first != same {
		t.Fatal("identical canonical requests produced different hashes")
	}
	if first == otherOperation || first == otherPayload {
		t.Fatal("request hash did not separate operation and payload")
	}
}
