package tapsdk

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/taproot-assets/tappsbt"
	"github.com/stretchr/testify/require"
)

// testAnchorFieldsPSBT returns a serialized unsigned PSBT with one input
// and the given number of outputs.
func testAnchorFieldsPSBT(t *testing.T, numOutputs int) []byte {
	t.Helper()

	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{})
	for range numOutputs {
		tx.AddTxOut(wire.NewTxOut(1_000, make([]byte, 34)))
	}

	packet, err := psbt.NewFromUnsignedTx(tx)
	require.NoError(t, err)

	encoded, err := serializePSBT(packet)
	require.NoError(t, err)

	return encoded
}

// TestAnchorOutputTaprootAssetRootRoundTrip ensures a root recorded with
// SetAnchorOutputTaprootAssetRoot is recovered by
// AnchorOutputTaprootAssetRoot, and that unrelated outputs stay bare.
func TestAnchorOutputTaprootAssetRootRoundTrip(t *testing.T) {
	t.Parallel()

	encoded := testAnchorFieldsPSBT(t, 2)

	var root Hash
	for i := range root {
		root[i] = byte(i + 1)
	}

	updated, err := SetAnchorOutputTaprootAssetRoot(encoded, 1, root)
	require.NoError(t, err)

	got, found, err := AnchorOutputTaprootAssetRoot(updated, 1)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, root, got)

	// The other output carries no root.
	got, found, err = AnchorOutputTaprootAssetRoot(updated, 0)
	require.NoError(t, err)
	require.False(t, found)
	require.Equal(t, Hash{}, got)
}

// TestAnchorOutputTaprootAssetRootAbsent ensures an output without the
// custom field reports found=false without an error.
func TestAnchorOutputTaprootAssetRootAbsent(t *testing.T) {
	t.Parallel()

	encoded := testAnchorFieldsPSBT(t, 1)

	got, found, err := AnchorOutputTaprootAssetRoot(encoded, 0)
	require.NoError(t, err)
	require.False(t, found)
	require.Equal(t, Hash{}, got)
}

// TestAnchorOutputTaprootAssetRootRejectsInvalid covers decode failures,
// out-of-range output indices, and malformed recorded values.
func TestAnchorOutputTaprootAssetRootRejectsInvalid(t *testing.T) {
	t.Parallel()

	encoded := testAnchorFieldsPSBT(t, 1)

	_, _, err := AnchorOutputTaprootAssetRoot(
		[]byte{0x01, 0x02, 0x03}, 0,
	)
	require.ErrorContains(t, err, "decoding anchor PSBT")

	_, _, err = AnchorOutputTaprootAssetRoot(encoded, 1)
	require.ErrorContains(t, err, "out of range")

	_, _, err = AnchorOutputTaprootAssetRoot(encoded, -1)
	require.ErrorContains(t, err, "out of range")

	_, err = SetAnchorOutputTaprootAssetRoot(
		[]byte{0x01, 0x02, 0x03}, 0, Hash{},
	)
	require.ErrorContains(t, err, "decoding anchor PSBT")

	_, err = SetAnchorOutputTaprootAssetRoot(encoded, 1, Hash{})
	require.ErrorContains(t, err, "out of range")

	// A recorded value with the wrong length is rejected on read.
	packet, err := psbt.NewFromRawBytes(bytes.NewReader(encoded), false)
	require.NoError(t, err)
	packet.Outputs[0].Unknowns = tappsbt.AddCustomField(
		packet.Outputs[0].Unknowns,
		tappsbt.PsbtKeyTypeOutputAssetRoot, make([]byte, 16),
	)
	malformed, err := serializePSBT(packet)
	require.NoError(t, err)

	_, _, err = AnchorOutputTaprootAssetRoot(malformed, 0)
	require.ErrorContains(t, err, "want 32")
}
