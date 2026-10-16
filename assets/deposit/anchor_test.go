package deposit

import (
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/commitment"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/taprpc"
	"github.com/lightninglabs/taproot-assets/tapscript"
	"github.com/stretchr/testify/require"
)

// hideDepositLeaves rebuilds the fixture's deposit anchor so it also commits
// to the hidden assets, which the deposit proof does not disclose. The deposit
// proof stays structurally valid. The returned commitment proves the hidden
// leaves.
func hideDepositLeaves(t *testing.T, f *witnessFixture,
	hidden ...*asset.Asset) *commitment.TapCommitment {

	t.Helper()

	version := commitment.TapCommitmentV2
	anchorCommitment, err := commitment.FromAssets(
		&version, append([]*asset.Asset{&f.proof.Asset}, hidden...)...,
	)
	require.NoError(t, err)
	sibling, err := f.kit.timeoutPathSibling()
	require.NoError(t, err)
	siblingHash, err := sibling.TapHash()
	require.NoError(t, err)
	script, err := tapscript.PayToAddrScript(
		*f.kit.muSig2Key.PreTweakedKey, siblingHash, *anchorCommitment,
	)
	require.NoError(t, err)
	_, inclusion, err := anchorCommitment.Proof(
		f.proof.Asset.TapCommitmentKey(),
		f.proof.Asset.AssetCommitmentKey(),
	)
	require.NoError(t, err)

	f.proof.AnchorTx.TxOut[1].PkScript = script
	f.prevOutputs[1].PkScript = script
	f.packet.UnsignedTx.TxIn[1].PreviousOutPoint = f.proof.OutPoint()
	f.proof.InclusionProof.CommitmentProof = &proof.CommitmentProof{
		Proof: *inclusion, TapSiblingPreimage: sibling,
	}
	_, err = f.proof.VerifyProofs()
	require.NoError(t, err)

	return anchorCommitment
}

// hiddenDepositAsset returns an OP_TRUE asset of another genesis for the
// fixture's deposit anchor.
func hiddenDepositAsset(f *witnessFixture) *asset.Asset {
	hidden := f.proof.Asset.Copy()
	hidden.Genesis.FirstPrevOut = wire.OutPoint{Hash: chainhash.Hash{0x77}}
	hidden.Amount = 100

	return hidden
}

// TestAcceptanceRequiresCompleteAnchor rejects deposits whose anchor commits
// to leaves missing from the proof through every acceptance entry point, as
// no later spend of the deposit could rebuild its input commitment.
func TestAcceptanceRequiresCompleteAnchor(t *testing.T) {
	tests := []struct {
		name   string
		hidden func(*witnessFixture) *asset.Asset
	}{
		{"hidden asset", hiddenDepositAsset},
		{"hidden tombstone", func(f *witnessFixture) *asset.Asset {
			tombstone := f.proof.Asset.Copy()
			tombstone.Amount = 0
			tombstone.ScriptKey = asset.NUMSScriptKey

			return tombstone
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newWitnessFixture(t)
			amount := f.proof.Asset.Amount
			_, err := f.kit.AnchorRootFromProofCommitment(
				f.proof, amount,
			)
			require.NoError(t, err)

			hideDepositLeaves(t, f, test.hidden(f))
			_, err = f.kit.AnchorRootFromProofCommitment(
				f.proof, amount,
			)
			require.ErrorContains(t, err, "incomplete deposit proof")
			verifier := &proofVerifierMock{
				response: &taprpc.VerifyProofResponse{Valid: true},
			}
			outpoint := f.proof.OutPoint()
			_, err = f.kit.VerifyProofFile(
				t.Context(), verifier, encodeProofFile(t, *f.proof),
				&outpoint, amount,
			)
			require.ErrorContains(t, err, "incomplete deposit proof")
			require.Equal(t, 1, verifier.calls)
		})
	}
}
