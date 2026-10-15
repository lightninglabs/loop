package htlc

import (
	"testing"

	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/stretchr/testify/require"
)

// TestProofVersionPolicy rejects future proof semantics at commitment-only
// entry points while retaining both explicitly supported input versions.
func TestProofVersionPolicy(t *testing.T) {
	for _, version := range []proof.TransitionVersion{
		proof.TransitionV0, proof.TransitionV1, 2,
	} {
		f := newWitnessFixture(t, SuccessSequence)
		f.proof.Version = version
		_, rootErr := GenTaprootAssetRootFromProof(f.proof)
		_, inputErr := f.kit.AssetInputIndex(f.proof, f.packet)
		if version == 2 {
			require.ErrorContains(t, rootErr, "unsupported transition proof version")
			require.ErrorContains(t, inputErr, "unsupported transition proof version")
			continue
		}
		require.NoError(t, rootErr)
		require.NoError(t, inputErr)
	}
}
