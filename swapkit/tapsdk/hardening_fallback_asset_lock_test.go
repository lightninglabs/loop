package tapsdk

import (
	"bytes"
	"testing"

	"github.com/lightninglabs/loop/swapkit"
	sdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/tappsbt"
	"github.com/lightninglabs/taproot-assets/tapsend"
	"github.com/stretchr/testify/require"
)

// TestFallbackRejectsAssetTimeLock checks that the fallback validator inspects
// the committed virtual output. A peer can give the HTLC asset an asset-level
// lock time far beyond the agreed refund height; the Bitcoin transaction and
// every summary still match the offer, but tapd would reject the asset
// transition at any confirmation height before the lock. The validator must
// reject the package rather than co-sign an unusable fallback.
func TestFallbackRejectsAssetTimeLock(t *testing.T) {
	f := newFallbackFixture(t)

	const lock = 5_000_000
	require.Greater(t, uint32(lock), f.offer.Terms.RefundHeight)

	forged := f.pkg.Clone()
	packet, err := tappsbt.Decode(forged.ActiveVirtualPsbts[0])
	require.NoError(t, err)
	require.Len(t, packet.Outputs, 1)
	output := packet.Outputs[0]
	output.LockTime = lock
	output.Asset.LockTime = lock

	// Recommit exactly as an author's backend would, without bubbling the
	// asset lock into the Bitcoin transaction.
	anchor, err := DecodePSBT(forged.CommittedAnchorPsbt)
	require.NoError(t, err)
	all := []*tappsbt.VPacket{packet}
	commitments, err := tapsend.CreateOutputCommitments(
		all, tapsend.WithNoSTXOProofs(),
	)
	require.NoError(t, err)
	require.NoError(t, tapsend.UpdateTaprootOutputKeys(
		anchor, packet, commitments,
	))
	anchor.UnsignedTx.LockTime = 0
	anchor.UnsignedTx.TxIn[0].Sequence = 0xfffffffc
	output.ProofSuffix, err = tapsend.CreateProofSuffix(
		anchor.UnsignedTx, anchor.Outputs, packet, commitments, 0, all,
		proof.WithVersion(output.ProofSuffix.Version),
	)
	require.NoError(t, err)
	forged.ActiveVirtualPsbts[0], err = tappsbt.Encode(packet)
	require.NoError(t, err)
	raw, err := EncodePSBT(anchor)
	require.NoError(t, err)
	forged.CommittedAnchorPsbt = raw
	forged.AnchorPsbt = bytes.Clone(raw)

	_, merkleRoot, assetRoot, err := tapsend.AnchorOutputScript(
		output.AnchorOutputInternalKey,
		output.AnchorOutputTapscriptSibling, commitments[0],
	)
	require.NoError(t, err)
	txid := anchor.UnsignedTx.TxHash()
	forged.Outputs[0].TaprootAssetRoot = sdk.Hash(assetRoot)
	forged.Outputs[0].TaprootMerkleRoot = sdk.Hash(merkleRoot)
	forged.Outputs[0].AnchorOutpoint.Txid = sdk.Hash(txid)
	forged.ProofUpdates[0].AnchorOutpoint.Txid = sdk.Hash(txid)
	var blob bytes.Buffer
	require.NoError(t, output.ProofSuffix.Encode(&blob))
	forged.ProofUpdates[0].ProofBlob = blob.Bytes()
	forged, err = forged.Seal()
	require.NoError(t, err)
	encoded, err := forged.MarshalBinary()
	require.NoError(t, err)

	// The committed HTLC asset carries a lock time the kit never agreed to.
	locked, err := proof.Decode(
		forged.ProofUpdates[0].ProofBlob,
	)
	require.NoError(t, err)
	require.EqualValues(t, lock, locked.Asset.LockTime)

	_, err = NewSwap(f.offer, f.deposit, encoded)
	require.ErrorIs(t, err, swapkit.ErrInvalid)
}
