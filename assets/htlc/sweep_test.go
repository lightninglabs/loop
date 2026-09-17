package htlc

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	assettest "github.com/lightninglabs/loop/assets/internal/test"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/commitment"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/tapscript"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// TestWitnessRejectsInvalidTransfers checks both signing paths reject omitted,
// corrupted and unanchored virtual transfers before asking for a signature.
func TestWitnessRejectsInvalidTransfers(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*witnessFixture)
	}{
		{"missing transfer", func(f *witnessFixture) {
			f.transfer = nil
		}},
		{"empty transfer", func(f *witnessFixture) {
			f.transfer.Packets = nil
		}},
		{"nil packet", func(f *witnessFixture) {
			f.transfer.Packets[0] = nil
		}},
		{"nil input", func(f *witnessFixture) {
			f.transfer.Packets[0].Inputs[0] = nil
		}},
		{"nil output", func(f *witnessFixture) {
			f.transfer.Packets[0].Outputs[0] = nil
		}},
		{"missing change", func(f *witnessFixture) {
			f.transfer.Packets[0].Outputs =
				f.transfer.Packets[0].Outputs[1:]
		}},
		{"Bitcoin-only output", func(f *witnessFixture) {
			f.packet.UnsignedTx.TxOut[1].PkScript =
				[]byte{txscript.OP_TRUE}
		}},
		{"missing Bitcoin output", func(f *witnessFixture) {
			f.packet.UnsignedTx.TxOut = f.packet.UnsignedTx.TxOut[:1]
			f.packet.Outputs = f.packet.Outputs[:1]
		}},
		{"output metadata", func(f *witnessFixture) {
			f.packet.Outputs = nil
		}},
		{"recipient mismatch", func(f *witnessFixture) {
			f.transfer.Packets[0].Outputs[1].ScriptKey =
				asset.NUMSScriptKey
		}},
		{"amount loss", func(f *witnessFixture) {
			out := f.transfer.Packets[0].Outputs[1]
			out.Amount--
			out.Asset.Amount--
		}},
		{"inflation", func(f *witnessFixture) {
			out := f.transfer.Packets[0].Outputs[1]
			out.Amount++
			out.Asset.Amount++
		}},
		{"invalid virtual witness", func(f *witnessFixture) {
			f.transfer.Packets[0].Outputs[0].Asset.
				PrevWitnesses[0].TxWitness = nil
		}},
		{"wrong input anchor", func(f *witnessFixture) {
			f.transfer.Packets[0].Inputs[0].PrevID.OutPoint.Index++
		}},
		{"duplicate packet", func(f *witnessFixture) {
			f.transfer.Packets = append(
				f.transfer.Packets, f.transfer.Packets[0],
			)
		}},
		{"prune live asset", func(f *witnessFixture) {
			f.transfer.PrunedAssets = map[wire.OutPoint][]*asset.Asset{
				f.proof.OutPoint(): {&f.proof.Asset},
			}
		}},
	}
	for _, timeout := range []bool{false, true} {
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				f := newWitnessFixture(t, SuccessSequence)
				signer := &localSigner{
					privateKey:    f.receiverKey,
					expectedInput: f.assetInIndex,
				}
				test.mutate(f)
				var err error
				if timeout {
					f.packet.UnsignedTx.TxIn[1].Sequence = 144
					signer.privateKey = f.senderKey
					_, err = f.kit.CreateTimeoutWitness(
						t.Context(), signer, f.proof, f.packet,
						keychain.KeyLocator{}, f.transfer,
					)
				} else {
					_, err = f.kit.CreatePreimageWitness(
						t.Context(), signer, f.proof, f.packet,
						keychain.KeyLocator{}, f.preimage,
						f.transfer,
					)
				}
				require.Error(t, err)
				require.Zero(t, signer.calls)
			})
		}
	}
}

// TestSweepPreservesPassiveAssets builds two assets in the same HTLC anchor.
// Both must be accounted for even though the kit describes only one of them.
func TestSweepPreservesPassiveAssets(t *testing.T) {
	f := newWitnessFixture(t, SuccessSequence)
	passive := f.proof.Asset.Copy()
	passive.Genesis.Tag = "passive asset"
	passive.Amount = 500
	tombstone := f.proof.Asset.Copy()
	tombstone.Amount = 0
	tombstone.ScriptKey = asset.NUMSScriptKey
	version := commitment.TapCommitmentV2
	inputCommitment, err := commitment.FromAssets(
		&version, &f.proof.Asset, passive, tombstone,
	)
	require.NoError(t, err)
	sibling, err := f.kit.GetSiblingPreimage()
	require.NoError(t, err)
	siblingHash, err := sibling.TapHash()
	require.NoError(t, err)
	internalKey, err := f.kit.GetAggregateKey()
	require.NoError(t, err)
	script, err := tapscript.PayToAddrScript(
		*internalKey, siblingHash, *inputCommitment,
	)
	require.NoError(t, err)
	f.proof.AnchorTx.TxOut[1].PkScript = script
	f.prevOutputs[1].PkScript = script
	f.packet.UnsignedTx.TxIn[1].PreviousOutPoint = f.proof.OutPoint()
	passiveProof := *f.proof
	passiveProof.Asset = *passive
	proofs := []*proof.Proof{f.proof, &passiveProof}
	for _, p := range proofs {
		_, cp, err := inputCommitment.Proof(
			p.Asset.TapCommitmentKey(), p.Asset.AssetCommitmentKey(),
		)
		require.NoError(t, err)
		p.InclusionProof.CommitmentProof = &proof.CommitmentProof{
			Proof: *cp, TapSiblingPreimage: &sibling,
		}
	}
	opTrue, err := GetOpTrueScript()
	require.NoError(t, err)
	_, _, _, cb, err := CreateOpTrueLeaf()
	require.NoError(t, err)
	control, err := cb.ToBytes()
	require.NoError(t, err)
	f.transfer = assettest.Sweep(
		t, proofs, f.packet, wire.TxWitness{opTrue, control},
	)
	f.transfer.PrunedAssets = map[wire.OutPoint][]*asset.Asset{
		f.proof.OutPoint(): {tombstone},
	}

	before := f.packet.UnsignedTx.TxHash()
	encodePackets := func() []byte {
		var buf bytes.Buffer
		for _, pkt := range f.transfer.Packets {
			require.NoError(t, pkt.Serialize(&buf))
		}
		return buf.Bytes()
	}
	packets := encodePackets()
	for range 2 {
		require.NoError(t, f.transfer.Validate(f.proof, f.packet))
	}
	require.Equal(t, before, f.packet.UnsignedTx.TxHash())
	require.Equal(t, packets, encodePackets())
	signer := &localSigner{
		privateKey: f.receiverKey, expectedInput: f.assetInIndex,
	}
	witness, err := f.kit.CreatePreimageWitness(
		t.Context(), signer, f.proof, f.packet, keychain.KeyLocator{},
		f.preimage, f.transfer,
	)
	require.NoError(t, err)
	f.executeWitness(t, witness)

	// Rebuilding both Bitcoin outputs without the passive transfer still
	// fails: the complete input commitment cannot be reconstructed.
	omitted := assettest.Sweep(
		t, proofs[:1], f.packet, wire.TxWitness{opTrue, control},
	)
	omitted.PrunedAssets = f.transfer.PrunedAssets
	require.ErrorContains(t, omitted.Validate(f.proof, f.packet),
		"invalid input commitments")
	omitted.PrunedAssets = map[wire.OutPoint][]*asset.Asset{
		f.proof.OutPoint(): {passive},
	}
	require.ErrorContains(t, omitted.Validate(f.proof, f.packet),
		"cannot prune spendable assets")
}

// TestSweepLegacyInputs retains refund support for existing commitment
// versions while new transfers use version-two commitments.
func TestSweepLegacyInputs(t *testing.T) {
	for _, version := range []asset.Version{asset.V0, asset.V1} {
		f := newWitnessFixtureWithVersions(
			t, 144, version, commitment.TapCommitmentVersion(version),
		)
		signer := &localSigner{
			privateKey: f.senderKey, expectedInput: f.assetInIndex,
		}
		witness, err := f.kit.CreateTimeoutWitness(
			t.Context(), signer, f.proof, f.packet,
			keychain.KeyLocator{}, f.transfer,
		)
		require.NoError(t, err)
		f.executeWitness(t, witness)
	}
}
