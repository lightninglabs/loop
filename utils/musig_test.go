package utils

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/input"
	"github.com/stretchr/testify/require"
)

// rawKeys returns serialized private keys for the given test seeds.
func rawKeys(seeds ...int32) [][32]byte {
	keys := make([][32]byte, len(seeds))
	for i, seed := range seeds {
		privKey, _ := test.CreateKey(seed)
		copy(keys[i][:], privKey.Serialize())
	}

	return keys
}

// signerPubKeys returns signer public keys derived from the given seeds in the
// format expected by the given MuSig2 version.
func signerPubKeys(t *testing.T, version input.MuSig2Version,
	seeds ...int32) []*btcec.PublicKey {

	t.Helper()

	pubKeys := make([]*btcec.PublicKey, len(seeds))
	for i, seed := range seeds {
		_, pubKey := test.CreateKey(seed)

		if version == input.MuSig2Version040 {
			var err error
			pubKey, err = schnorr.ParsePubKey(
				schnorr.SerializePubKey(pubKey),
			)
			require.NoError(t, err)
		}

		pubKeys[i] = pubKey
	}

	return pubKeys
}

// hasOddY returns true if the compressed serialization of the public key uses
// the odd-Y prefix.
func hasOddY(pubKey *btcec.PublicKey) bool {
	return pubKey.SerializeCompressed()[0] == 0x03
}

// TestMuSig2SignRejectsSingleSigner ensures the helper fails fast with a clear
// error instead of entering an invalid one-party MuSig2 flow.
func TestMuSig2SignRejectsSingleSigner(t *testing.T) {
	_, err := MuSig2Sign(
		input.MuSig2Version100RC2,
		rawKeys(1),
		&input.MuSig2Tweaks{},
		[32]byte{},
	)
	require.ErrorContains(t, err, "need at least two signing keys")
}

// TestMuSig2SignSupportsVersions verifies the helper works with the supported
// MuSig2 versions used in Loop.
func TestMuSig2SignSupportsVersions(t *testing.T) {
	t.Parallel()

	tweaks := &input.MuSig2Tweaks{}
	msg := [32]byte{1}

	testCases := []struct {
		name       string
		version    input.MuSig2Version
		seeds      []int32
		oddYSigner int32
	}{
		{
			name:    testVersionName(input.MuSig2Version040),
			version: input.MuSig2Version040,
			seeds:   []int32{1, 2},
		},
		{
			name:    testVersionName(input.MuSig2Version100RC2),
			version: input.MuSig2Version100RC2,
			seeds:   []int32{1, 2},
		},
		{
			name: testVersionName(input.MuSig2Version040) +
				" odd Y",
			version:    input.MuSig2Version040,
			seeds:      []int32{5, 1},
			oddYSigner: 5,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			if testCase.oddYSigner != 0 {
				_, oddYKey := test.CreateKey(
					testCase.oddYSigner,
				)
				require.True(t, hasOddY(oddYKey))
			}

			keys := rawKeys(testCase.seeds...)
			pubKeys := signerPubKeys(
				t, testCase.version, testCase.seeds...,
			)

			sigBytes, err := MuSig2Sign(
				testCase.version, keys, tweaks, msg,
			)
			require.NoError(t, err)
			require.Len(t, sigBytes, 64)

			sig, err := schnorr.ParseSignature(sigBytes)
			require.NoError(t, err)

			combinedKey, err := input.MuSig2CombineKeys(
				testCase.version, pubKeys, true, tweaks,
			)
			require.NoError(t, err)
			require.True(
				t, sig.Verify(msg[:], combinedKey.FinalKey),
			)
		})
	}
}

// testVersionName returns a stable subtest name for a MuSig2 version.
func testVersionName(version input.MuSig2Version) string {
	switch version {
	case input.MuSig2Version040:
		return "MuSig2 0.4"

	case input.MuSig2Version100RC2:
		return "MuSig2 1.0RC2"

	default:
		return "unknown"
	}
}
