package utils

import (
	"math/big"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/input"
	"github.com/stretchr/testify/require"
)

// musig2Versions lists the MuSig2 versions that MuSig2Sign supports.
var musig2Versions = []input.MuSig2Version{
	input.MuSig2Version040,
	input.MuSig2Version100RC2,
}

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
		pubKeys[i] = versionedPubKey(t, version, pubKey)
	}

	return pubKeys
}

// versionedPubKey returns the public key in the format expected by the given
// MuSig2 version.
func versionedPubKey(t *testing.T, version input.MuSig2Version,
	pubKey *btcec.PublicKey) *btcec.PublicKey {

	t.Helper()

	// MuSig2 v0.4 expects x-only public keys.
	if version != input.MuSig2Version040 {
		return pubKey
	}

	xOnlyPubKey, err := schnorr.ParsePubKey(
		schnorr.SerializePubKey(pubKey),
	)
	require.NoError(t, err)

	return xOnlyPubKey
}

// hasOddY returns true if the compressed serialization of the public key uses
// the odd-Y prefix.
func hasOddY(pubKey *btcec.PublicKey) bool {
	return pubKey.SerializeCompressed()[0] == 0x03
}

// requireValidSig asserts that sigBytes is a valid Schnorr signature of msg
// under the MuSig2 combination of the given signer public keys.
func requireValidSig(t *testing.T, version input.MuSig2Version,
	pubKeys []*btcec.PublicKey, tweaks *input.MuSig2Tweaks, msg [32]byte,
	sigBytes []byte) {

	t.Helper()

	require.Len(t, sigBytes, 64)

	sig, err := schnorr.ParseSignature(sigBytes)
	require.NoError(t, err)

	combinedKey, err := input.MuSig2CombineKeys(
		version, pubKeys, true, tweaks,
	)
	require.NoError(t, err)
	require.True(t, sig.Verify(msg[:], combinedKey.FinalKey))
}

// TestMuSig2SignRejectsTooFewSigners ensures the helper fails fast with a clear
// error instead of entering an invalid one-party MuSig2 flow. The signer count
// is checked before any key is parsed, so the error must not depend on the
// MuSig2 version or on the keys passed.
func TestMuSig2SignRejectsTooFewSigners(t *testing.T) {
	t.Parallel()

	const wantErr = "need at least two signing keys"

	testCases := []struct {
		name string
		keys [][32]byte
	}{
		{
			name: "no keys",
		},
		{
			name: "one valid key",
			keys: rawKeys(1),
		},
		{
			name: "one zero key",
			keys: make([][32]byte, 1),
		},
	}

	for _, version := range musig2Versions {
		for _, testCase := range testCases {
			name := testVersionName(version) + " " + testCase.name
			t.Run(name, func(t *testing.T) {
				t.Parallel()

				_, err := MuSig2Sign(
					version, testCase.keys,
					&input.MuSig2Tweaks{}, [32]byte{},
				)
				require.ErrorContains(t, err, wantErr)
			})
		}
	}
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
			requireValidSig(
				t, testCase.version, pubKeys, tweaks, msg,
				sigBytes,
			)
		})
	}
}

// TestMuSig2SignNormalizesOverflowingKey verifies that a raw key above the
// group order signs as its value modulo the group order, as documented on
// MuSig2Sign.
func TestMuSig2SignNormalizesOverflowingKey(t *testing.T) {
	t.Parallel()

	var overflowingKey [32]byte
	for i := range overflowingKey {
		overflowingKey[i] = 0xff
	}

	// Reduce the key with math/big instead of btcec, so the expected
	// public key doesn't depend on the normalization under test. The
	// reduction must change the key, otherwise it wasn't out of range.
	var normalizedKey [32]byte
	new(big.Int).Mod(
		new(big.Int).SetBytes(overflowingKey[:]), btcec.S256().N,
	).FillBytes(normalizedKey[:])
	require.NotEqual(t, overflowingKey, normalizedKey)

	_, normalizedPubKey := btcec.PrivKeyFromBytes(normalizedKey[:])
	_, otherPubKey := test.CreateKey(2)
	keys := [][32]byte{overflowingKey, rawKeys(2)[0]}

	tweaks := &input.MuSig2Tweaks{}
	msg := [32]byte{1}

	for _, version := range musig2Versions {
		t.Run(testVersionName(version), func(t *testing.T) {
			t.Parallel()

			sigBytes, err := MuSig2Sign(version, keys, tweaks, msg)
			require.NoError(t, err)

			pubKeys := []*btcec.PublicKey{
				versionedPubKey(t, version, normalizedPubKey),
				versionedPubKey(t, version, otherPubKey),
			}
			requireValidSig(
				t, version, pubKeys, tweaks, msg, sigBytes,
			)
		})
	}
}

// TestMuSig2SignRejectsZeroKeys verifies that the two raw keys that normalize
// to zero, zero itself and the group order, are rejected with an error instead
// of a panic.
func TestMuSig2SignRejectsZeroKeys(t *testing.T) {
	t.Parallel()

	const wantErr = "signing key 1 is zero modulo the group order"

	var groupOrderKey [32]byte
	btcec.S256().N.FillBytes(groupOrderKey[:])

	testCases := []struct {
		name string
		key  [32]byte
	}{
		{
			name: "zero key",
			key:  [32]byte{},
		},
		{
			name: "group order key",
			key:  groupOrderKey,
		},
	}

	tweaks := &input.MuSig2Tweaks{}
	msg := [32]byte{1}

	for _, version := range musig2Versions {
		for _, testCase := range testCases {
			name := testVersionName(version) + " " + testCase.name
			t.Run(name, func(t *testing.T) {
				t.Parallel()

				// Pass the bad key second, so the error has to
				// name the right signer.
				keys := [][32]byte{rawKeys(1)[0], testCase.key}

				var err error
				require.NotPanics(t, func() {
					_, err = MuSig2Sign(
						version, keys, tweaks, msg,
					)
				})
				require.ErrorContains(t, err, wantErr)
			})
		}
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
