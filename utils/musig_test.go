package utils

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/input"
	"github.com/stretchr/testify/require"
)

// TestMuSig2SignRejectsSingleSigner ensures the helper fails fast with a clear
// error instead of entering an invalid one-party MuSig2 flow.
func TestMuSig2SignRejectsSingleSigner(t *testing.T) {
	privKey, pubKey := test.CreateKey(1)

	_, err := MuSig2Sign(
		input.MuSig2Version100RC2,
		[]*btcec.PrivateKey{privKey},
		[]*btcec.PublicKey{pubKey},
		&input.MuSig2Tweaks{},
		[32]byte{},
	)
	require.ErrorContains(t, err, "need at least two signing keys")
}

// TestMuSig2SignRejectsMismatchedKeyCounts ensures we fail before creating any
// MuSig2 sessions when the signer sets are inconsistent.
func TestMuSig2SignRejectsMismatchedKeyCounts(t *testing.T) {
	privKey, pubKey1 := test.CreateKey(1)
	_, pubKey2 := test.CreateKey(2)

	_, err := MuSig2Sign(
		input.MuSig2Version100RC2,
		[]*btcec.PrivateKey{privKey},
		[]*btcec.PublicKey{pubKey1, pubKey2},
		&input.MuSig2Tweaks{},
		[32]byte{},
	)
	require.ErrorContains(t, err, "must match number of public keys")
}
