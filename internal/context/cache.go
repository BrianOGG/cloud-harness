package context

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// PrefixHash is the identity of the stable prefix. Equal hashes mean the
// system instructions and tool digest were not reordered or rewritten.
func PrefixHash(msgs []Message) string {
	raw, err := json.Marshal(msgs)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// HitRate is cached input tokens divided by input tokens.
// A zero input returns 0.
func HitRate(cached, input int) float64 {
	if input <= 0 {
		return 0
	}
	return float64(cached) / float64(input)
}
