package tapsdk

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/lightninglabs/taproot-assets/tappsbt"
)

// AnchorOutputTaprootAssetRoot returns the Taproot Asset commitment root
// that tapd records as a custom PSBT field on the asset-carrying outputs
// of a committed anchor transaction. The boolean reports whether the
// output carries the field. An error is returned when the PSBT cannot be
// decoded, the output index is out of range, or the recorded value is not
// a 32-byte root.
//
// Integrators that persist or relay committed anchor PSBTs can use this
// to recover an output's commitment root without depending on tapd's
// internal PSBT conventions.
func AnchorOutputTaprootAssetRoot(anchorPSBT []byte,
	outputIndex int) (Hash, bool, error) {

	var root Hash

	packet, err := psbt.NewFromRawBytes(
		bytes.NewReader(anchorPSBT), false,
	)
	if err != nil {
		return root, false, fmt.Errorf("decoding anchor PSBT: %w", err)
	}

	if outputIndex < 0 || outputIndex >= len(packet.Outputs) {
		return root, false, fmt.Errorf("anchor output index %d out "+
			"of range, PSBT has %d outputs", outputIndex,
			len(packet.Outputs))
	}

	value := tappsbt.ExtractCustomField(
		packet.Outputs[outputIndex].Unknowns,
		tappsbt.PsbtKeyTypeOutputAssetRoot,
	)
	if len(value) == 0 {
		return root, false, nil
	}

	if len(value) != len(root) {
		return root, false, fmt.Errorf("anchor output %d Taproot "+
			"Asset root has %d bytes, want %d", outputIndex,
			len(value), len(root))
	}

	copy(root[:], value)

	return root, true, nil
}

// SetAnchorOutputTaprootAssetRoot records the given Taproot Asset
// commitment root on the anchor PSBT output at the given index, using the
// same custom PSBT field tapd writes when committing an anchor
// transaction. It returns the re-serialized PSBT. This is intended for
// template tooling and test fixtures; in normal operation tapd records
// the field itself.
func SetAnchorOutputTaprootAssetRoot(anchorPSBT []byte, outputIndex int,
	root Hash) ([]byte, error) {

	packet, err := psbt.NewFromRawBytes(
		bytes.NewReader(anchorPSBT), false,
	)
	if err != nil {
		return nil, fmt.Errorf("decoding anchor PSBT: %w", err)
	}

	if outputIndex < 0 || outputIndex >= len(packet.Outputs) {
		return nil, fmt.Errorf("anchor output index %d out of "+
			"range, PSBT has %d outputs", outputIndex,
			len(packet.Outputs))
	}

	packet.Outputs[outputIndex].Unknowns = tappsbt.AddCustomField(
		packet.Outputs[outputIndex].Unknowns,
		tappsbt.PsbtKeyTypeOutputAssetRoot, root[:],
	)

	return serializePSBT(packet)
}
