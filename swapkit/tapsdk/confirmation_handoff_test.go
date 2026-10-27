package tapsdk

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/wire/v2"
	sdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/stretchr/testify/require"
)

type handoffFixture struct {
	*pathFixture

	child         *sdk.CustomAnchorTransferPackage
	binding       InputBinding
	confirmations map[sdk.Hash]sdk.AnchorConfirmation
}

func newHandoffFixture(t *testing.T) handoffFixture {
	t.Helper()
	f := newPathFixture(t)
	_, parent := f.parent(t)
	request, binding := f.child(t, parent)
	parents, err := NewApprovedParents(t.Context(), f.wallet, f.chain, 2,
		request, []InputBinding{binding}, []ParentApproval{{Package: parent,
			Digest: parent.CommittedPackageDigest}})
	require.NoError(t, err)
	builder := f.wallet.NewCustomAnchorTxBuilder()
	builder.SetConfirmedProofVerifier(parents)
	plan, err := builder.Build(t.Context(), request)
	require.NoError(t, err)
	child, err := plan.Commit(t.Context(), sdk.CustomAnchorCommitOptions{})
	require.NoError(t, err)
	child = pathFinal(t, child)
	packet, err := DecodePSBT(parent.AnchorPsbt)
	require.NoError(t, err)
	tx, err := psbt.Extract(packet)
	require.NoError(t, err)
	block := &wire.MsgBlock{Header: wire.BlockHeader{MerkleRoot: tx.TxHash()}, Transactions: []*wire.MsgTx{tx}}
	f.chain.blocks[150] = block
	for _, input := range tx.TxIn {
		delete(f.chain.utxos, input.PreviousOutPoint)
	}
	for i, output := range tx.TxOut {
		f.chain.utxos[wire.OutPoint{Hash: tx.TxHash(), Index: uint32(i)}] = output
	}
	var raw bytes.Buffer
	require.NoError(t, block.Serialize(&raw))
	f.client.verifies = 0
	return handoffFixture{pathFixture: f, child: child, binding: binding,
		confirmations: map[sdk.Hash]sdk.AnchorConfirmation{sdk.Hash(tx.TxHash()): {BlockHeight: 150, Block: raw.Bytes()}}}
}

// TestConfirmPackageInputsNativePublish preserves every approved transaction,
// asset and policy field while installing native confirmed input histories.
// The real SDK publisher accepts the replacement without new signatures; its
// backend is a mock and publication/import retry state remains caller-owned.
func TestConfirmPackageInputsNativePublish(t *testing.T) {
	f := newHandoffFixture(t)
	before := f.child.Clone()
	updated, files, err := ConfirmPackageInputs(t.Context(), f.wallet, f.chain, 2,
		f.child, f.child.CommittedPackageDigest, f.confirmations, []InputBinding{f.binding})
	require.NoError(t, err)
	require.Equal(t, before, f.child)
	require.NoError(t, checkConfirmationOnly(before, updated))
	require.NotEqual(t, before.CommittedPackageDigest, updated.CommittedPackageDigest)
	require.Equal(t, before.AnchorPsbt, updated.AnchorPsbt)
	require.Equal(t, before.Outputs, updated.Outputs)
	require.Len(t, files, 1)
	require.Equal(t, 1, f.client.verifies)
	var path sdk.AssetProofPath
	require.NoError(t, path.UnmarshalBinary(updated.Inputs[0].ProofSource.Blob))
	require.Empty(t, path.Steps)
	require.Equal(t, files[0].RawProofFile, path.ConfirmedBaseProof)
	p, err := LastProof(files[0].RawProofFile)
	require.NoError(t, err)
	require.Equal(t, sdk.Outpoint{Txid: sdk.Hash(p.Asset.Genesis.FirstPrevOut.Hash),
		Index: p.Asset.Genesis.FirstPrevOut.Index}, files[0].GenesisPoint)
	_, err = f.wallet.PublishCustomAnchorTransfer(t.Context(), before, before.AnchorPsbt)
	require.ErrorIs(t, err, sdk.ErrUnconfirmedCustomAnchorPublish)
	require.Zero(t, f.client.publishes)
	_, err = f.wallet.PublishCustomAnchorTransfer(t.Context(), updated, updated.AnchorPsbt)
	require.ErrorContains(t, err, "publication is outside this test")
	require.Equal(t, 1, f.client.publishes)
	f.client.verifies = 0
	retried, _, err := ConfirmPackageInputs(t.Context(), f.wallet, f.chain, 2,
		updated, updated.CommittedPackageDigest, nil, []InputBinding{f.binding})
	require.NoError(t, err)
	require.Equal(t, updated.CommittedPackageDigest, retried.CommittedPackageDigest)
	require.Equal(t, 1, f.client.verifies)
}

func TestConfirmPackageInputsRejectsChanges(t *testing.T) {
	const spentDuringVerification = "spent during verify"
	for _, name := range []string{"saved digest", "unsigned child", "missing confirmation",
		"fake block", "wrong asset", "spent fee input", testSpoofedFeeValue,
		"spoofed fee script", spentDuringVerification} {
		t.Run(name, func(t *testing.T) {
			f := newHandoffFixture(t)
			digest := f.child.CommittedPackageDigest
			switch name {
			case "saved digest":
				digest[0] ^= 1
			case "unsigned child":
				f.child = f.child.Clone()
				f.child.AnchorPsbt = f.child.CommittedAnchorPsbt
				var err error
				f.child, err = f.child.Seal()
				require.NoError(t, err)
			case "missing confirmation":
				f.confirmations = nil
			case "fake block":
				for id, c := range f.confirmations {
					var block wire.MsgBlock
					require.NoError(t, block.Deserialize(bytes.NewReader(c.Block)))
					block.Header.Nonce++
					var raw bytes.Buffer
					require.NoError(t, block.Serialize(&raw))
					c.Block = raw.Bytes()
					f.confirmations[id] = c
				}
			case "wrong asset":
				f.binding.Amount++
			case "spent fee input":
				p, err := DecodePSBT(f.child.AnchorPsbt)
				require.NoError(t, err)
				delete(f.chain.utxos, p.UnsignedTx.TxIn[0].PreviousOutPoint)
			case testSpoofedFeeValue, "spoofed fee script":
				p, err := DecodePSBT(f.child.AnchorPsbt)
				require.NoError(t, err)
				point := p.UnsignedTx.TxIn[0].PreviousOutPoint
				if name == testSpoofedFeeValue {
					f.chain.utxos[point].Value++
				} else {
					f.chain.utxos[point].PkScript = []byte{1}
				}
			case spentDuringVerification:
				f.client.afterVerify = func() { clear(f.chain.utxos) }
			}
			updated, files, err := ConfirmPackageInputs(t.Context(), f.wallet, f.chain, 2,
				f.child, digest, f.confirmations, []InputBinding{f.binding})
			require.Error(t, err)
			require.Nil(t, updated)
			require.Nil(t, files)
			if name != spentDuringVerification {
				require.Zero(t, f.client.verifies)
			}
		})
	}
}

func TestConfirmationSnapshotRejectsOtherMetadata(t *testing.T) {
	f := newHandoffFixture(t)
	for _, mutate := range []func(*sdk.CustomAnchorTransferPackage){
		func(p *sdk.CustomAnchorTransferPackage) { p.Publish.Label += "changed" },
		func(p *sdk.CustomAnchorTransferPackage) { p.Outputs[0].Amount++ },
	} {
		changed := f.child.Clone()
		mutate(changed)
		require.Error(t, checkConfirmationOnly(f.child, changed))
	}
	// A proof for another input state cannot be installed as metadata.
	p, err := proof.Decode(f.child.ProofUpdates[0].ProofBlob)
	require.NoError(t, err)
	p.Asset.Amount++
	require.Error(t, replaceInputProof(f.child.Clone(), f.child.Inputs[0], p))
}

func TestConfirmationSnapshotsBeforeRPC(t *testing.T) {
	f := newHandoffFixture(t)
	f.client.afterInfo = func() {
		f.binding.Anchor.Leaves[0][0] ^= 1
		clear(f.confirmations)
	}
	_, files, err := ConfirmPackageInputs(t.Context(), f.wallet, f.chain, 2,
		f.child, f.child.CommittedPackageDigest, f.confirmations, []InputBinding{f.binding})
	require.NoError(t, err)
	require.Len(t, files, 1)
}
