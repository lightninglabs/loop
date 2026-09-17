package test

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/assets/sweep"
	"github.com/lightninglabs/taproot-assets/address"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/tappsbt"
	"github.com/lightninglabs/taproot-assets/tapsend"
	"github.com/stretchr/testify/require"
)

// Sweep anchors complete non-interactive transfers with change into btc. Each
// proof is a separate asset transfer, potentially sharing its Bitcoin anchor.
// The witness must spend each input's asset script key.
func Sweep(t testing.TB, proofs []*proof.Proof, btc *psbt.Packet,
	witness wire.TxWitness) *sweep.Transfer {

	t.Helper()
	transfer := &sweep.Transfer{}
	for idx, p := range proofs {
		pkt, err := tappsbt.FromProofs(
			[]*proof.Proof{p}, &address.RegressionNetTap, tappsbt.V1,
		)
		require.NoError(t, err)
		_, dest := btcec.PrivKeyFromBytes([]byte{byte(20 + idx*2)})
		_, change := btcec.PrivKeyFromBytes([]byte{byte(21 + idx*2)})
		_, anchor := btcec.PrivKeyFromBytes([]byte{30})
		changeAmount := p.Asset.Amount / 5
		pkt.Outputs = []*tappsbt.VOutput{
			{
				Type:   tappsbt.TypeSplitRoot,
				Amount: changeAmount, AssetVersion: asset.V1,
				ScriptKey:               asset.NewScriptKey(change),
				AnchorOutputInternalKey: anchor,
				AnchorOutputIndex:       0,
			},
			{
				Amount:                  p.Asset.Amount - changeAmount,
				AssetVersion:            asset.V1,
				ScriptKey:               asset.NewScriptKey(dest),
				AnchorOutputInternalKey: anchor,
				AnchorOutputIndex:       1,
			},
		}
		require.NoError(t, tapsend.PrepareOutputAssets(t.Context(), pkt))
		for _, out := range pkt.Outputs {
			require.NoError(t, out.Asset.UpdateTxWitness(0, witness))
			if out.SplitAsset != nil {
				require.NoError(t, out.SplitAsset.UpdateTxWitness(
					0, witness,
				))
			}
		}
		transfer.Packets = append(transfer.Packets, pkt)
		for i, in := range btc.UnsignedTx.TxIn {
			if in.PreviousOutPoint == p.OutPoint() {
				btc.Inputs[i].TaprootInternalKey =
					schnorr.SerializePubKey(
						p.InclusionProof.InternalKey,
					)
			}
		}
	}
	commitments, err := tapsend.CreateOutputCommitments(transfer.Packets)
	require.NoError(t, err)
	anchor, err := tapsend.CreateAnchorTx(transfer.Packets)
	require.NoError(t, err)
	for _, pkt := range transfer.Packets {
		require.NoError(t, tapsend.UpdateTaprootOutputKeys(
			anchor, pkt, commitments,
		))
	}
	btc.UnsignedTx.TxOut = anchor.UnsignedTx.TxOut
	btc.Outputs = anchor.Outputs
	return transfer
}
