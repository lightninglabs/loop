package codec

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/stretchr/testify/require"
)

func TestEncodeAltLeaf(t *testing.T) {
	privKey, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{1}, 32))
	pubKeyBytes := privKey.PubKey().SerializeCompressed()

	var scriptKey [33]byte
	copy(scriptKey[:], pubKeyBytes)

	leafBytes, err := EncodeAltLeaf(0x1234, scriptKey)
	require.NoError(t, err)

	expected := append([]byte{0x0e, 0x02, 0x12, 0x34, 0x10, 0x21}, pubKeyBytes...)
	require.Equal(t, expected, leafBytes)
}

func TestEncodeAltLeafInvalidKey(t *testing.T) {
	_, err := EncodeAltLeaf(0, [33]byte{})
	require.Error(t, err)
}

func TestAltLeavesRoundTrip(t *testing.T) {
	leaves := [][]byte{
		{0x01, 0x02, 0x03},
		{0xaa},
	}

	encoded, err := EncodeAltLeaves(leaves)
	require.NoError(t, err)

	decoded, err := DecodeAltLeaves(encoded)
	require.NoError(t, err)
	require.Equal(t, leaves, decoded)
}
