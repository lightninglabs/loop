package htlc

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	assettest "github.com/lightninglabs/loop/assets/internal/test"
	"github.com/lightninglabs/loop/assets/sweep"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/commitment"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/tapscript"
	"github.com/lightninglabs/taproot-assets/tapsend"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

const (
	preimagePath = "preimage"
	timeoutPath  = "timeout"
)

// TestWitnessRejectsInvalidExclusions checks the entire Bitcoin transaction,
// including undeclared outputs and STXOs duplicated into another asset anchor.
func TestWitnessRejectsInvalidExclusions(t *testing.T) {
	duplicateOutput := func(f *witnessFixture, index int) {
		out := f.packet.UnsignedTx.TxOut[index]
		f.packet.UnsignedTx.AddTxOut(&wire.TxOut{
			Value: 330, PkScript: bytes.Clone(out.PkScript),
		})
		f.packet.Outputs = append(f.packet.Outputs,
			f.packet.Outputs[index])
	}
	tests := []struct {
		name   string
		mutate func(*testing.T, *witnessFixture)
	}{
		{"duplicate root output", func(_ *testing.T, f *witnessFixture) {
			duplicateOutput(f, 0)
		}},
		{"duplicate recipient output", func(_ *testing.T, f *witnessFixture) {
			duplicateOutput(f, 1)
		}},
		{"missing internal key", func(_ *testing.T, f *witnessFixture) {
			duplicateOutput(f, 0)
			f.packet.Outputs[2] = psbt.POutput{}
		}},
		{"incorrect internal key", func(t *testing.T, f *witnessFixture) {
			duplicateOutput(f, 0)
			_, other := scalarKey(t, 42)
			f.packet.Outputs[2].TaprootInternalKey =
				schnorr.SerializePubKey(other)
		}},
		{"malformed tap tree", func(_ *testing.T, f *witnessFixture) {
			duplicateOutput(f, 0)
			f.packet.Outputs[2].TaprootTapTree = []byte{0}
		}},
		{"hidden asset tap tree", func(t *testing.T, f *witnessFixture) {
			duplicateOutput(f, 0)
			commitments, err := tapsend.CreateOutputCommitments(
				f.transfer.Packets, tapsend.WithNoSTXOProofs(),
			)
			require.NoError(t, err)
			leaf := commitments[0].TapLeaf()
			var tree bytes.Buffer
			require.NoError(t, tree.WriteByte(0))
			require.NoError(t, tree.WriteByte(byte(leaf.LeafVersion)))
			require.NoError(t, wire.WriteVarBytes(&tree, 0, leaf.Script))
			f.packet.Outputs[2].TaprootTapTree = tree.Bytes()
		}},
		{"STXO in both anchors", func(t *testing.T, f *witnessFixture) {
			outputs := f.transfer.Packets[0].Outputs
			outputs[1].AltLeaves = append(outputs[1].AltLeaves,
				outputs[0].AltLeaves...)
			assettest.Anchor(t, f.transfer, f.packet)
		}},
	}
	for _, timeout := range []bool{false, true} {
		path := preimagePath
		if timeout {
			path = timeoutPath
		}
		for _, test := range tests {
			t.Run(path+"/"+test.name, func(t *testing.T) {
				f := newWitnessFixture(t, SuccessSequence)
				commitments, err := tapsend.CreateOutputCommitments(
					f.transfer.Packets, tapsend.WithNoSTXOProofs(),
				)
				require.NoError(t, err)

				// Valid cached suffixes describe the old transaction.
				// Signing must check fresh proofs for the final outputs.
				pkt := f.transfer.Packets[0]
				for idx, out := range pkt.Outputs {
					out.ProofSuffix, err = tapsend.CreateProofSuffix(
						f.packet.UnsignedTx, f.packet.Outputs,
						pkt, commitments, idx, f.transfer.Packets,
					)
					require.NoError(t, err)
					_, err = out.ProofSuffix.VerifyProofs()
					require.NoError(t, err)
				}
				test.mutate(t, f)
				// Approve this Bitcoin layout so the asset exclusion
				// checks still reject the hidden commitments themselves.
				f.transfer = sweep.NewTransfer(
					f.transfer.Packets, f.packet.UnsignedTx.TxOut,
				)
				signer := &localSigner{
					privateKey:    f.receiverKey,
					expectedInput: f.assetInIndex,
				}
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
				require.ErrorContains(t, err, "asset output proof")
				require.Zero(t, signer.calls)
			})
		}
	}
}

// TestSweepAllowsBitcoinOutputs ensures ordinary Bitcoin change remains valid
// when the PSBT supplies the metadata needed to prove Taproot asset exclusion.
func TestSweepAllowsBitcoinOutputs(t *testing.T) {
	for _, outputType := range []string{"P2WPKH", "BIP86", "tapscript"} {
		t.Run(outputType, func(t *testing.T) {
			f := newWitnessFixture(t, SuccessSequence)
			_, key := scalarKey(t, 42)
			var (
				script []byte
				meta   psbt.POutput
				err    error
			)
			switch outputType {
			case "P2WPKH":
				script, err = txscript.NewScriptBuilder().
					AddOp(txscript.OP_0).AddData(make([]byte, 20)).
					Script()
			case "BIP86":
				meta.TaprootInternalKey = schnorr.SerializePubKey(key)
				script, err = txscript.PayToTaprootScript(
					txscript.ComputeTaprootKeyNoScript(key),
				)
			case "tapscript":
				leaf := txscript.NewBaseTapLeaf([]byte{txscript.OP_TRUE})
				root := leaf.TapHash()
				meta.TaprootInternalKey = schnorr.SerializePubKey(key)
				var tree bytes.Buffer
				require.NoError(t, tree.WriteByte(0))
				require.NoError(t, tree.WriteByte(byte(leaf.LeafVersion)))
				require.NoError(t, wire.WriteVarBytes(&tree, 0, leaf.Script))
				meta.TaprootTapTree = tree.Bytes()
				script, err = txscript.PayToTaprootScript(
					txscript.ComputeTaprootOutputKey(key, root[:]),
				)
			}
			require.NoError(t, err)
			f.packet.UnsignedTx.AddTxOut(&wire.TxOut{
				Value: 330, PkScript: script,
			})
			f.packet.Outputs = append(f.packet.Outputs, meta)
			f.transfer = sweep.NewTransfer(
				f.transfer.Packets, f.packet.UnsignedTx.TxOut,
			)
			var before bytes.Buffer
			require.NoError(t, f.packet.Serialize(&before))
			for range 2 {
				require.NoError(t, f.transfer.Validate(f.proof, f.packet))
			}
			var after bytes.Buffer
			require.NoError(t, f.packet.Serialize(&after))
			require.Equal(t, before.Bytes(), after.Bytes())
			signer := &localSigner{
				privateKey: f.receiverKey, expectedInput: f.assetInIndex,
			}
			witness, err := f.kit.CreatePreimageWitness(
				t.Context(), signer, f.proof, f.packet,
				keychain.KeyLocator{}, f.preimage, f.transfer,
			)
			require.NoError(t, err)
			f.executeWitness(t, witness)
		})
	}
}

// TestWitnessRejectsInvalidTransitionCommitments checks activation-required
// leaves even when the Bitcoin outputs faithfully commit to the invalid leaves.
func TestWitnessRejectsInvalidTransitionCommitments(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *sweep.Transfer)
	}{
		{"missing all leaves", func(_ *testing.T, transfer *sweep.Transfer) {
			transfer.Packets[0].Outputs[0].AltLeaves = nil
		}},
		{"missing spender leaves", func(t *testing.T, transfer *sweep.Transfer) {
			out := transfer.Packets[0].Outputs[0]
			var err error
			out.AltLeaves, err = asset.CollectSTXO(out.Asset)
			require.NoError(t, err)
		}},
		{"missing STXO leaves", func(t *testing.T, transfer *sweep.Transfer) {
			out := transfer.Packets[0].Outputs[0]
			var err error
			out.AltLeaves, err = asset.CollectSpenders(out.Asset)
			require.NoError(t, err)
		}},
		{"incorrect spender", func(_ *testing.T, transfer *sweep.Transfer) {
			out := transfer.Packets[0].Outputs[0]
			leaves := asset.FromAltLeaves(out.AltLeaves)
			for _, leaf := range leaves {
				if len(leaf.PrevWitnesses) != 0 {
					leaf.PrevWitnesses[0].TxWitness[0][0] ^= 1
				}
			}
		}},
		{"incorrect STXO payload", func(_ *testing.T, transfer *sweep.Transfer) {
			out := transfer.Packets[0].Outputs[0]
			leaves := asset.FromAltLeaves(out.AltLeaves)
			for _, leaf := range leaves {
				if len(leaf.PrevWitnesses) == 0 {
					leaf.PrevWitnesses = []asset.Witness{{
						TxWitness: wire.TxWitness{{1}},
					}}
				}
			}
		}},
		{"leaves in wrong anchor output", func(_ *testing.T, transfer *sweep.Transfer) {
			outputs := transfer.Packets[0].Outputs
			outputs[1].AltLeaves = outputs[0].AltLeaves
			outputs[0].AltLeaves = nil
		}},
	}
	for _, timeout := range []bool{false, true} {
		path := preimagePath
		if timeout {
			path = timeoutPath
		}
		for _, test := range tests {
			t.Run(path+"/"+test.name, func(t *testing.T) {
				f := newWitnessFixture(t, SuccessSequence)
				test.mutate(t, f.transfer)
				assettest.Anchor(t, f.transfer, f.packet)
				signer := &localSigner{
					privateKey:    f.receiverKey,
					expectedInput: f.assetInIndex,
				}
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
				require.ErrorContains(t, err, "transition commitment")
				require.Zero(t, signer.calls)
			})
		}
	}
}

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
	// Leaves may be carried by another virtual output sharing the same
	// Bitcoin anchor. Validation must check the combined commitment.
	first := f.transfer.Packets[0].Outputs[0]
	second := f.transfer.Packets[1].Outputs[0]
	second.AltLeaves = append(second.AltLeaves, first.AltLeaves...)
	first.AltLeaves = nil

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

// newHtlcSpend builds a sweep of a funded HTLC behind an unrelated fee input,
// with the asset transfer to the receiver's destination already anchored.
func newHtlcSpend(t *testing.T, f *witnessFixture, htlcProof *proof.Proof,
	sequence uint32) *witnessFixture {

	t.Helper()

	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Hash: chainhash.Hash{0x42}},
		Sequence:         wire.MaxTxInSequenceNum,
	})
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: htlcProof.OutPoint(),
		Sequence:         sequence,
	})
	packet, err := psbt.NewFromUnsignedTx(tx)
	require.NoError(t, err)
	htlcOutput := htlcProof.AnchorTx.TxOut[htlcProof.InclusionProof.OutputIndex]
	prevOutputs := []*wire.TxOut{
		{Value: 3000, PkScript: []byte{txscript.OP_TRUE}},
		{Value: htlcOutput.Value, PkScript: bytes.Clone(htlcOutput.PkScript)},
	}
	for idx := range prevOutputs {
		packet.Inputs[idx].WitnessUtxo = prevOutputs[idx]
	}
	transfer := assettest.Sweep(
		t, []*proof.Proof{htlcProof}, packet, opTrueWitness(t),
	)

	return &witnessFixture{
		transfer: transfer, kit: f.kit, proof: htlcProof, packet: packet,
		prevOutputs: prevOutputs, senderKey: f.senderKey,
		receiverKey: f.receiverKey, preimage: f.preimage,
		assetInIndex: 1,
	}
}

// TestAcceptedHtlcsRemainClaimable funds HTLCs in every honest layout with
// tapd's own coin distribution and proofs. Each passes acceptance, and both
// the receiver's claim and the sender's refund can be signed.
func TestAcceptedHtlcsRemainClaimable(t *testing.T) {
	paths := []struct {
		name    string
		timeout bool
	}{{"claim", false}, {"refund", true}}
	for _, layout := range fundingLayouts() {
		for _, path := range paths {
			t.Run(layout.name+"/"+path.name, func(t *testing.T) {
				f := newWitnessFixture(t, SuccessSequence)
				htlcProof := fundHtlcProof(t, f, layout)
				_, err := f.kit.GetPkScriptFromProof(htlcProof)
				require.NoError(t, err)

				var witness *SpendWitness
				if path.timeout {
					spend := newHtlcSpend(
						t, f, htlcProof, f.kit.CsvExpiry(),
					)
					witness, err = spend.kit.CreateTimeoutWitness(
						t.Context(), &localSigner{
							privateKey:    spend.senderKey,
							expectedInput: 1,
						}, spend.proof, spend.packet,
						keychain.KeyLocator{}, spend.transfer,
					)
					require.NoError(t, err)
					spend.executeWitness(t, witness)

					return
				}

				spend := newHtlcSpend(t, f, htlcProof, SuccessSequence)
				witness, err = spend.kit.CreatePreimageWitness(
					t.Context(), &localSigner{
						privateKey:    spend.receiverKey,
						expectedInput: 1,
					}, spend.proof, spend.packet,
					keychain.KeyLocator{}, spend.preimage,
					spend.transfer,
				)
				require.NoError(t, err)
				spend.executeWitness(t, witness)
			})
		}
	}
}

// TestHiddenAnchorLeafBlocksClaim shows why acceptance rejects incomplete
// proofs. A receiver that knows only the proven asset cannot sign its claim,
// while the funder, which knows the hidden leaf, can still refund after the
// CSV delay.
func TestHiddenAnchorLeafBlocksClaim(t *testing.T) {
	f := newWitnessFixture(t, SuccessSequence)
	hidden := hiddenAsset(f)
	anchorCommitment := hideAnchorLeaves(t, f, hidden)
	_, err := f.kit.GetPkScriptFromProof(f.proof)
	require.ErrorContains(t, err, "does not match the proven asset")

	claim := assettest.Sweep(
		t, []*proof.Proof{f.proof}, f.packet, opTrueWitness(t),
	)
	claimSigner := &localSigner{
		privateKey: f.receiverKey, expectedInput: f.assetInIndex,
	}
	_, err = f.kit.CreatePreimageWitness(
		t.Context(), claimSigner, f.proof, f.packet,
		keychain.KeyLocator{}, f.preimage, claim,
	)
	require.ErrorContains(t, err, "invalid input commitments")
	require.Zero(t, claimSigner.calls)

	hiddenProof := *f.proof
	hiddenProof.Asset = *hidden
	_, inclusion, err := anchorCommitment.Proof(
		hidden.TapCommitmentKey(), hidden.AssetCommitmentKey(),
	)
	require.NoError(t, err)
	hiddenProof.InclusionProof.CommitmentProof = &proof.CommitmentProof{
		Proof: *inclusion,
		TapSiblingPreimage: f.proof.InclusionProof.CommitmentProof.
			TapSiblingPreimage,
	}
	f.packet.UnsignedTx.TxIn[f.assetInIndex].Sequence = f.kit.CsvExpiry()
	refund := assettest.Sweep(
		t, []*proof.Proof{f.proof, &hiddenProof}, f.packet,
		opTrueWitness(t),
	)
	refundSigner := &localSigner{
		privateKey: f.senderKey, expectedInput: f.assetInIndex,
	}
	witness, err := f.kit.CreateTimeoutWitness(
		t.Context(), refundSigner, f.proof, f.packet,
		keychain.KeyLocator{}, refund,
	)
	require.NoError(t, err)
	f.executeWitness(t, witness)
}
