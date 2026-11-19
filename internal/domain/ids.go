package domain

import (
	"crypto/rand"
	"encoding/hex"
)

// NewID returns prefix plus 32 hex characters from crypto/rand.
// Prefixes used by the harness include ses_, trn_, run_, stp_, rsp_, snap_, bat_, inv_, evt_.
func NewID(prefix string) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("crypto/rand: " + err.Error())
	}
	return prefix + hex.EncodeToString(b[:])
}
