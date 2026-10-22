package deposit

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/txscript/v2"
	assettest "github.com/lightninglabs/loop/assets/internal/test"
	"github.com/lightninglabs/taproot-assets/address"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/tappsbt"
	"github.com/lightninglabs/taproot-assets/tapsend"
	"github.com/stretchr/testify/require"
)

// TestTimeoutAssetTimelock ensures the deposit signing entry point also rejects
// an anchor whose refund sequence is shorter than the new asset output's lock.
func TestTimeoutAssetTimelock(t *testing.T) {
	for _, test := range []struct {
		name     string
		relative uint64
		valid    bool
	}{
		{name: "insufficient relative", relative: 145},
		{name: "matching relative", relative: 144, valid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newWitnessFixture(t)
			recipient := f.transfer.Packets[0].Outputs[1]
			pkt, err := tappsbt.FromProofs(
				[]*proof.Proof{f.proof}, &address.RegressionNetTap,
				tappsbt.V1,
			)
			require.NoError(t, err)
			pkt.Outputs = []*tappsbt.VOutput{{
				Amount: f.proof.Asset.Amount, AssetVersion: asset.V1,
				ScriptKey: recipient.ScriptKey, Interactive: true,
				AnchorOutputIndex:       0,
				AnchorOutputInternalKey: recipient.AnchorOutputInternalKey,
				RelativeLockTime:        test.relative,
			}}
			require.NoError(t, tapsend.PrepareOutputAssets(t.Context(), pkt))
			require.NoError(t, pkt.Outputs[0].Asset.UpdateTxWitness(
				0, opTrueWitness(t),
			))
			f.transfer.Packets[0] = pkt
			_, err = tapsend.CreateOutputCommitments(
				f.transfer.Packets, tapsend.WithSpenderLeaves(),
			)
			require.NoError(t, err)
			assettest.Anchor(t, f.transfer, f.packet)
			require.Zero(t, f.proof.Asset.RelativeLockTime)

			encodeState := func() ([]byte, []byte) {
				var psbt, virtual bytes.Buffer
				require.NoError(t, f.packet.Serialize(&psbt))
				require.NoError(t, pkt.Serialize(&virtual))
				return psbt.Bytes(), virtual.Bytes()
			}
			beforePSBT, beforeVirtual := encodeState()
			beforeTxid := f.packet.UnsignedTx.TxHash()
			signer := &localSigner{
				privateKey: f.funderKey, expectedInput: f.assetInIndex,
			}
			witness, err := f.kit.CreateTimeoutWitness(
				t.Context(), signer, f.proof, f.packet, f.transfer,
			)
			afterPSBT, afterVirtual := encodeState()
			require.Equal(t, beforePSBT, afterPSBT)
			require.Equal(t, beforeVirtual, afterVirtual)
			require.Equal(t, beforeTxid, f.packet.UnsignedTx.TxHash())
			require.Equal(t, uint32(144),
				f.packet.UnsignedTx.TxIn[f.assetInIndex].Sequence)
			if !test.valid {
				require.ErrorContains(t, err, "asset timelock")
				require.Zero(t, signer.calls)
				require.Nil(t, witness)
				return
			}
			require.NoError(t, err)
			require.Equal(t, 1, signer.calls)

			tx := f.packet.UnsignedTx
			tx.TxIn[witness.InputIndex].Witness = witness.Witness
			fetcher := txscript.NewMultiPrevOutFetcher(nil)
			for idx, in := range tx.TxIn {
				fetcher.AddPrevOut(in.PreviousOutPoint, f.prevOutputs[idx])
			}
			prevOutput := f.prevOutputs[witness.InputIndex]
			engine, err := txscript.NewEngine(
				prevOutput.PkScript, tx, int(witness.InputIndex),
				txscript.StandardVerifyFlags, nil,
				txscript.NewTxSigHashes(tx, fetcher), prevOutput.Value,
				fetcher,
			)
			require.NoError(t, err)
			require.NoError(t, engine.Execute())
		})
	}
}
