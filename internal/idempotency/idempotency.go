package idempotency

import "crypto/sha256"

const (
	OrderCreateOperation   = "orders.create.v1"
	PaymentCreateOperation = "payments.create.v1"
	MaximumKeyLength       = 128
)

func ValidKey(key string) bool {
	if len(key) < 1 || len(key) > MaximumKeyLength || !isAlphaNumeric(key[0]) {
		return false
	}
	for index := 1; index < len(key); index++ {
		character := key[index]
		if !isAlphaNumeric(character) && character != '.' && character != '_' && character != ':' && character != '-' {
			return false
		}
	}
	return true
}

func KeyHash(key string) [sha256.Size]byte {
	return sha256.Sum256([]byte(key))
}

func RequestHash(operation string, canonicalPayload []byte) [sha256.Size]byte {
	hasher := sha256.New()
	_, _ = hasher.Write([]byte(operation))
	_, _ = hasher.Write([]byte{0})
	_, _ = hasher.Write(canonicalPayload)
	var digest [sha256.Size]byte
	copy(digest[:], hasher.Sum(nil))
	return digest
}

func isAlphaNumeric(character byte) bool {
	return (character >= 'a' && character <= 'z') ||
		(character >= 'A' && character <= 'Z') ||
		(character >= '0' && character <= '9')
}
