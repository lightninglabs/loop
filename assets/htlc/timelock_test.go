package htlc

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/txscript"
	assettest "github.com/lightninglabs/loop/assets/internal/test"
	"github.com/lightninglabs/taproot-assets/address"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/tappsbt"
	"github.com/lightninglabs/taproot-assets/tapsend"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// newTimelockedWitnessFixture adds a lock to a full-value virtual output. The
// input asset remains unlocked, so the new transition requires the Bitcoin
// anchor to enforce the output's lock even though its virtual witness is valid.
func newTimelockedWitnessFixture(t *testing.T, sequence uint32,
	absolute, relative uint64) *witnessFixture {

	t.Helper()

	f := newWitnessFixture(t, sequence)
	recipient := f.transfer.Packets[0].Outputs[1]
	pkt, err := tappsbt.FromProofs(
		[]*proof.Proof{f.proof}, &address.RegressionNetTap, tappsbt.V1,
	)
	require.NoError(t, err)
	pkt.Outputs = []*tappsbt.VOutput{{
		Amount: f.proof.Asset.Amount, AssetVersion: asset.V1,
		ScriptKey: recipient.ScriptKey, Interactive: true,
		AnchorOutputIndex:       0,
		AnchorOutputInternalKey: recipient.AnchorOutputInternalKey,
		LockTime:                absolute,
		RelativeLockTime:        relative,
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
	require.Zero(t, f.proof.Asset.LockTime)
	require.Zero(t, f.proof.Asset.RelativeLockTime)
	require.Zero(t, f.packet.UnsignedTx.LockTime)
	require.Equal(t, sequence,
		f.packet.UnsignedTx.TxIn[f.assetInIndex].Sequence)

	return f
}

// signTimelockFeeInput places a real fee signature in the PSBT before the asset
// signer runs. The returned check verifies that it still signs the same anchor.
func signTimelockFeeInput(t *testing.T, f *witnessFixture) func() {
	t.Helper()

	privateKey, publicKey := scalarKey(t, 42)
	outputKey := txscript.ComputeTaprootKeyNoScript(publicKey)
	script, err := txscript.PayToTaprootScript(outputKey)
	require.NoError(t, err)
	f.prevOutputs[0].PkScript = script
	tx := f.packet.UnsignedTx
	fetcher := txscript.NewMultiPrevOutFetcher(nil)
	for idx, in := range tx.TxIn {
		fetcher.AddPrevOut(in.PreviousOutPoint, f.prevOutputs[idx])
	}
	sig, err := txscript.RawTxInTaprootSignature(
		tx, txscript.NewTxSigHashes(tx, fetcher), 0,
		f.prevOutputs[0].Value, script, nil, txscript.SigHashDefault,
		privateKey,
	)
	require.NoError(t, err)
	f.packet.Inputs[0].TaprootKeySpendSig = sig
	parsed, err := schnorr.ParseSignature(sig)
	require.NoError(t, err)

	return func() {
		t.Helper()

		digest, err := txscript.CalcTaprootSignatureHash(
			txscript.NewTxSigHashes(tx, fetcher), txscript.SigHashDefault,
			tx, 0, fetcher,
		)
		require.NoError(t, err)
		require.True(t, parsed.Verify(digest, outputKey))
	}
}

// TestWitnessAssetTimelocks requires both HTLC signing paths to reject anchors
// that could confirm before the asset transition is valid. Matching locks must
// remain claimable without changing the transaction or its existing signature.
func TestWitnessAssetTimelocks(t *testing.T) {
	paths := []struct {
		name     string
		timeout  bool
		sequence uint32
	}{
		{name: preimagePath, sequence: SuccessSequence},
		{name: timeoutPath, timeout: true, sequence: 144},
	}
	for _, path := range paths {
		tests := []struct {
			name       string
			absolute   uint64
			relative   uint64
			anchorLock uint32
			valid      bool
		}{
			{
				name: "insufficient absolute", absolute: 1_000_000,
			},
			{
				name: "matching absolute", absolute: 1_000_000,
				anchorLock: 1_000_000, valid: true,
			},
			{
				name:     "insufficient relative",
				relative: uint64(path.sequence) + 1,
			},
			{
				name:     "matching relative",
				relative: uint64(path.sequence), valid: true,
			},
		}
		for _, test := range tests {
			t.Run(path.name+"/"+test.name, func(t *testing.T) {
				f := newTimelockedWitnessFixture(
					t, path.sequence, test.absolute, test.relative,
				)
				f.packet.UnsignedTx.LockTime = test.anchorLock
				checkFeeSignature := signTimelockFeeInput(t, f)
				checkFeeSignature()

				encodeState := func() ([]byte, []byte) {
					var psbt, virtual bytes.Buffer
					require.NoError(t, f.packet.Serialize(&psbt))
					for _, pkt := range f.transfer.Packets {
						require.NoError(t, pkt.Serialize(&virtual))
					}
					return psbt.Bytes(), virtual.Bytes()
				}
				beforePSBT, beforeVirtual := encodeState()
				beforeTxid := f.packet.UnsignedTx.TxHash()
				signer := &localSigner{
					privateKey:    f.receiverKey,
					expectedInput: f.assetInIndex,
				}
				var (
					witness *SpendWitness
					err     error
				)
				if path.timeout {
					signer.privateKey = f.senderKey
					witness, err = f.kit.CreateTimeoutWitness(
						t.Context(), signer, f.proof, f.packet,
						keychain.KeyLocator{}, f.transfer,
					)
				} else {
					witness, err = f.kit.CreatePreimageWitness(
						t.Context(), signer, f.proof, f.packet,
						keychain.KeyLocator{}, f.preimage, f.transfer,
					)
				}
				afterPSBT, afterVirtual := encodeState()
				require.Equal(t, beforePSBT, afterPSBT)
				require.Equal(t, beforeVirtual, afterVirtual)
				require.Equal(t, beforeTxid, f.packet.UnsignedTx.TxHash())
				require.Equal(t, path.sequence,
					f.packet.UnsignedTx.TxIn[f.assetInIndex].Sequence)
				checkFeeSignature()
				if !test.valid {
					require.ErrorContains(t, err, "asset timelock")
					require.Zero(t, signer.calls)
					require.Nil(t, witness)
					return
				}
				require.NoError(t, err)
				require.Equal(t, 1, signer.calls)
				f.executeWitness(t, witness)
				checkFeeSignature()
			})
		}
	}
}
