package tapsdk

import (
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/stretchr/testify/require"
)

// TestParseGroupRefKeyPreservesCompressedKey ensures a 33-byte compressed
// group key keeps its exact bytes, including the parity byte for odd-y
// keys. Silently normalizing through schnorr parsing would flip odd-y
// keys to the even-y curve point and break semantic lookups.
func TestParseGroupRefKeyPreservesCompressedKey(t *testing.T) {
	t.Parallel()

	oddKey := oddCompressedPubKey(t)
	hexKey := hex.EncodeToString(oddKey[:])

	parsed, err := ParseGroupRefKey(hexKey)
	require.NoError(t, err)
	require.Equal(t, oddKey, parsed)
}

// TestParseGroupRefKeyXOnlyFallback ensures the helper accepts 32-byte
// x-only encodings for the paths where tapd surfaces the tweaked form.
func TestParseGroupRefKeyXOnlyFallback(t *testing.T) {
	t.Parallel()

	oddKey := oddCompressedPubKey(t)
	xOnlyHex := hex.EncodeToString(oddKey[1:])
	expected, err := ParseTaprootPubKey(oddKey[1:])
	require.NoError(t, err)

	parsed, err := ParseGroupRefKey(xOnlyHex)
	require.NoError(t, err)
	require.Equal(t, expected, parsed)
}

// TestParseGroupRefKeyRejectsBadInputs guards the two boundary errors
// callers may hit at an RPC seam: malformed hex and wrong-length payload.
func TestParseGroupRefKeyRejectsBadInputs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
	}{
		{name: "invalid hex", input: "nothex"},
		{name: testEmpty, input: ""},
		{name: "wrong length", input: "deadbeef"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := ParseGroupRefKey(test.input)
			require.Error(t, err)
		})
	}
}

func oddCompressedPubKey(t *testing.T) PubKey {
	t.Helper()

	for i := byte(1); i < 64; i++ {
		privKey, _ := btcec.PrivKeyFromBytes([]byte{i})
		compressed := privKey.PubKey().SerializeCompressed()
		if compressed[0] != 0x03 {
			continue
		}

		parsed, err := ParsePubKey(compressed)
		require.NoError(t, err)

		xOnly := schnorr.SerializePubKey(privKey.PubKey())
		require.Equal(t, xOnly, parsed[1:])

		return parsed
	}

	t.Fatal("failed to derive odd compressed pubkey")
	return PubKey{}
}
