package tapsdk

import (
	"context"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	sdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/taproot-assets/commitment"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/stretchr/testify/require"
)

type observedRefundSDK struct {
	*observedRecoverySDK

	backend *pathClient
}

func (c *observedRefundSDK) CommitVirtualPsbtsWithRequest(
	ctx context.Context, request *sdk.CommitVirtualPsbtsRequest) (
	*sdk.CommitVirtualPsbtsResponse, error) {

	return c.backend.CommitVirtualPsbtsWithRequest(ctx, request)
}

// TestObservedCLTVRefundPreservesTransaction reconstructs a valid historical
// non-RBF refund after new refund templates have started opting into RBF.
func TestObservedCLTVRefundPreservesTransaction(t *testing.T) {
	f := newPathFixture(t)
	p, err := LastProof(f.request.Inputs[0].ProofFile)
	require.NoError(t, err)
	root, _, err := isolatedRoots(p)
	require.NoError(t, err)
	spend, err := f.kit.TimeoutSpend(root)
	require.NoError(t, err)
	require.EqualValues(t, 0xfffffffd, spend.Sequence)

	// Mint the fixture's same asset under the actual HTLC anchor policy.
	plan := f.kit.AnchorPlan()
	internal, err := btcec.ParsePubKey(plan.InternalKey[:])
	require.NoError(t, err)
	branch := txscript.NewTapBranch(
		txscript.NewBaseTapLeaf(plan.Leaves[0]),
		txscript.NewBaseTapLeaf(plan.Leaves[1]),
	)
	sibling := commitment.NewPreimageFromBranch(branch)
	p.InclusionProof.InternalKey = internal
	p.InclusionProof.CommitmentProof.TapSiblingPreimage = &sibling
	p.AnchorTx.TxOut[0].PkScript = spend.PkScript
	p.BlockHeader.MerkleRoot = p.AnchorTx.TxHash()
	inputRaw, err := proof.EncodeAsProofFile(p)
	require.NoError(t, err)
	f.chain.blocks[p.BlockHeight] = bindingBlock(p)
	point := sdk.Outpoint{Txid: sdk.Hash(p.OutPoint().Hash),
		Index: p.OutPoint().Index}
	_, optrue, err := LegacyOPTrueDescriptor()
	require.NoError(t, err)
	prior := sdk.CustomAnchorAssetOutputSummary{
		LogicalOutputID: "observed refund",
		AssetRef:        f.request.Inputs[0].AssetRef,
		IssuanceID:      sdk.AssetID(p.Asset.ID()),
		Amount:          p.Asset.Amount, ScriptKey: f.binding.ScriptKey,
		OPTrueSpend: optrue, TaprootAssetRoot: root,
		TaprootMerkleRoot: spend.TaprootMerkleRoot,
		AnchorValueSat:    p.AnchorTx.TxOut[0].Value,
		AnchorOutpoint:    point,
	}
	client := &observedRefundSDK{
		observedRecoverySDK: &observedRecoverySDK{
			partialRecoverySDK: &partialRecoverySDK{
				bindingSDK: &bindingSDK{valid: true},
			},
			input: &sdk.ProofFile{RawProofFile: inputRaw},
			point: point, stopErr: errors.New("reached observer log"),
		},
		backend: f.client,
	}
	wallet := sdk.NewWallet(client, sdk.NetworkRegtest)
	public := bindingPub(1)
	recipient, err := walletOutput(public, 0, 0,
		prior.AssetRef, prior.Amount, 900)
	require.NoError(t, err)
	anchor, err := AnchorTemplate(p, recipient.Anchor.InternalKey, 900, spend)
	require.NoError(t, err)
	packet, err := DecodePSBT(anchor)
	require.NoError(t, err)
	packet.UnsignedTx.LockTime = spend.LockTime + 1
	packet.UnsignedTx.TxIn[0].Sequence = wire.MaxTxInSequenceNum - 1
	anchor, err = EncodePSBT(packet)
	require.NoError(t, err)
	input, err := AssetInput(prior, inputRaw)
	require.NoError(t, err)
	pkg, err := commit(t.Context(), wallet, f.chain, 1,
		CustomRequest(input, recipient, anchor, ScriptSigningPlan(spend, 0)))
	require.NoError(t, err)
	requests, err := pkg.SigningRequests()
	require.NoError(t, err)
	require.Len(t, requests.ScriptPath, 1)
	signature, err := schnorr.Sign(
		bindingKey(1), requests.ScriptPath[0].Sighash[:],
	)
	require.NoError(t, err)
	final, err := pkg.ApplyScriptPathWitness(requests.ScriptPath[0].ID,
		[][]byte{signature.Serialize()})
	require.NoError(t, err)
	packet, err = DecodePSBT(final.AnchorPsbt)
	require.NoError(t, err)
	observed, err := psbt.Extract(packet)
	require.NoError(t, err)

	_, err = rebuildObservedSpend(t.Context(), wallet, f.chain, 1,
		prior, inputRaw, public, spend, observed)
	require.ErrorIs(t, err, client.stopErr)
	require.NotNil(t, client.logged)
	require.True(t, client.logged.SkipAnchorTxBroadcast)
	logged, err := DecodePSBT(client.logged.AnchorPsbt)
	require.NoError(t, err)
	recovered, err := psbt.Extract(logged)
	require.NoError(t, err)
	require.Equal(t, observed.TxHash(), recovered.TxHash())
	require.Equal(t, observed.LockTime, recovered.LockTime)
	require.Equal(t, observed.Version, recovered.Version)
	require.EqualValues(t, 0xfffffffe, recovered.TxIn[0].Sequence)
	require.Equal(t, observed.TxIn[0].Witness, recovered.TxIn[0].Witness)
}
