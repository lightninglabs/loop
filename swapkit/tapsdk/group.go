package tapsdk

import (
	"bytes"

	"github.com/btcsuite/btcd/btcec/v2"
)

// sameGroupKeys compares valid compressed group keys by their x-only identity.
// It does not normalize or replace the caller's serialized key metadata.
func sameGroupKeys(first, second []byte) bool {
	if len(first) != btcec.PubKeyBytesLenCompressed ||
		len(second) != btcec.PubKeyBytesLenCompressed {

		return false
	}
	if _, err := btcec.ParsePubKey(first); err != nil {
		return false
	}
	if _, err := btcec.ParsePubKey(second); err != nil {
		return false
	}
	return bytes.Equal(first[1:], second[1:])
}
