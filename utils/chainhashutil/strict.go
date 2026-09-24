package chainhashutil

import (
	"fmt"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
)

// NewHashFromStrExact parses a chainhash string that must be fully specified.
func NewHashFromStrExact(hash string) (chainhash.Hash, error) {
	if len(hash) != chainhash.MaxHashStringSize {
		return chainhash.Hash{}, fmt.Errorf(
			"invalid hash string length of %v, want %v",
			len(hash), chainhash.MaxHashStringSize)
	}

	parsed, err := chainhash.NewHashFromStr(hash)
	if err != nil {
		return chainhash.Hash{}, err
	}

	// chainhash.NewHashFromStr uses a pointer return, but on success it
	// returns a populated hash, not (nil, nil), so dereferencing here is
	// safe.
	return *parsed, nil
}
