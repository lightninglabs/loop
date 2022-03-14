package test

import (
	"github.com/btcsuite/btcd/btcec/v2"
)

// CreateKey returns a deterministically generated key pair.
func CreateKey(index int32) (*btcec.PrivateKey, *btcec.PublicKey) {
	// Avoid all zeros, because it results in an invalid key.
	privKey, pubKey := btcec.PrivKeyFromBytes([]byte{
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, byte(index + 1),
	})

	return privKey, pubKey
}
