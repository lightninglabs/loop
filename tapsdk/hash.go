package tapsdk

import (
	"encoding/hex"
	"fmt"
)

// Hash is a 32-byte hash value.
type Hash [32]byte

// ParseHash parses a 32-byte hash from raw bytes. An empty slice returns the
// zero hash.
func ParseHash(b []byte) (Hash, error) {
	var h Hash
	if len(b) == 0 {
		return h, nil
	}

	if len(b) != len(h) {
		return h, fmt.Errorf("hash must be %d bytes, was %d", len(h),
			len(b))
	}

	copy(h[:], b)
	return h, nil
}

// ParseHashHex parses a hex-encoded 32-byte hash. An empty string returns the
// zero hash.
func ParseHashHex(s string) (Hash, error) {
	if s == "" {
		return Hash{}, nil
	}

	b, err := hex.DecodeString(s)
	if err != nil {
		return Hash{}, err
	}

	return ParseHash(b)
}

// String returns the hex encoding of the hash.
func (h Hash) String() string {
	return hex.EncodeToString(h[:])
}
