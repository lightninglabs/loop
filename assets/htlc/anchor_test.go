package htlc

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/taproot-assets/address"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/commitment"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/tappsbt"
	"github.com/lightninglabs/taproot-assets/tapscript"
	"github.com/lightninglabs/taproot-assets/tapsend"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// fundingLayout describes how a funder anchors an HTLC output.
type fundingLayout struct {
	name        string
	interactive bool
	source      uint64
}

// fundingLayouts returns the interactive and address funding layouts, with and
// without asset change, for the fixture's 1000-unit HTLC.
func fundingLayouts() []fundingLayout {
	return []fundingLayout{
		{name: "interactive exact amount", interactive: true, source: 1000},
		{name: "interactive with change", interactive: true, source: 1500},
		{name: "address exact amount", source: 1000},
		{name: "address with change", source: 1500},
	}
}

// opTrueWitness returns the virtual witness that spends an OP_TRUE asset.
func opTrueWitness(t *testing.T) wire.TxWitness {
	t.Helper()

	script, err := GetOpTrueScript()
	require.NoError(t, err)
	_, _, _, controlBlock, err := CreateOpTrueLeaf()
	require.NoError(t, err)
	control, err := controlBlock.ToBytes()
	require.NoError(t, err)

	return wire.TxWitness{script, control}
}

// sourceProof anchors a single asset in a plain Taproot Asset output.
func sourceProof(t *testing.T, sourceAsset *asset.Asset) *proof.Proof {
	t.Helper()

	version := commitment.TapCommitmentV2
	sourceCommitment, err := commitment.FromAssets(&version, sourceAsset)
	require.NoError(t, err)
	_, inclusion, err := sourceCommitment.Proof(
		sourceAsset.TapCommitmentKey(), sourceAsset.AssetCommitmentKey(),
	)
	require.NoError(t, err)
	_, internalKey := scalarKey(t, 0x92)
	script, err := tapscript.PayToAddrScript(
		*internalKey, nil, *sourceCommitment,
	)
	require.NoError(t, err)

	anchorTx := wire.NewMsgTx(2)
	anchorTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: sourceAsset.Genesis.FirstPrevOut,
	})
	anchorTx.AddTxOut(&wire.TxOut{Value: 1000, PkScript: script})

	return &proof.Proof{
		AnchorTx: *anchorTx,
		Asset:    *sourceAsset,
		InclusionProof: proof.TaprootProof{
			OutputIndex: 0,
			InternalKey: internalKey,
			CommitmentProof: &proof.CommitmentProof{
				Proof: *inclusion,
			},
		},
	}
}

// fundHtlcProof funds the fixture's HTLC from an OP_TRUE source asset with
// tapd's coin distribution and proof generation, and returns tapd's proof for
// the HTLC output. Interactive funding places the HTLC alone at output zero,
// as the reservation fallback does. Address funding uses the split-root layout
// of a send to an HTLC address.
func fundHtlcProof(t *testing.T, f *witnessFixture,
	layout fundingLayout) *proof.Proof {

	t.Helper()

	opTrueKey, _, _, _, err := CreateOpTrueLeaf()
	require.NoError(t, err)
	htlcScriptKey := asset.NewScriptKey(opTrueKey.PubKey)
	source, err := asset.New(
		f.proof.Asset.Genesis, layout.source, 0, 0, htlcScriptKey, nil,
		asset.WithAssetVersion(asset.V1),
	)
	require.NoError(t, err)

	var template *tappsbt.VPacket
	if layout.interactive {
		template, err = f.kit.CreateInteractiveHtlcVpkt()
		require.NoError(t, err)
		htlcOutput := template.Outputs[1]
		htlcOutput.AnchorOutputIndex = 0
		template.Outputs = []*tappsbt.VOutput{htlcOutput}
	} else {
		internalKey, err := f.kit.GetAggregateKey()
		require.NoError(t, err)
		sibling, err := f.kit.GetSiblingPreimage()
		require.NoError(t, err)
		template, err = tappsbt.FromAddresses([]*address.Tap{{
			Version:          address.V1,
			AssetVersion:     asset.V1,
			ChainParams:      &address.RegressionNetTap,
			AssetID:          f.kit.AssetID(),
			Amount:           f.kit.Amount(),
			ScriptKey:        *htlcScriptKey.PubKey,
			InternalKey:      *internalKey,
			TapscriptSibling: &sibling,
		}}, 1)
		require.NoError(t, err)
	}

	allocations, interactive, err := tapsend.AllocationsFromTemplate(
		template, layout.source,
	)
	require.NoError(t, err)
	packets, err := tapsend.DistributeCoins(
		[]*proof.Proof{sourceProof(t, source)}, allocations,
		&address.RegressionNetTap, interactive, template.Version,
	)
	require.NoError(t, err)
	require.Len(t, packets, 1)
	packet := packets[0]

	// Supply the change keys that the funder's tapd wallet would derive.
	_, changeKey := scalarKey(t, 0x93)
	_, changeAnchorKey := scalarKey(t, 0x94)
	for _, output := range packet.Outputs {
		if !output.Type.IsSplitRoot() {
			continue
		}
		if output.Amount > 0 {
			output.ScriptKey = asset.NewScriptKey(changeKey)
		}
		if output.AnchorOutputInternalKey == nil {
			output.AnchorOutputInternalKey = changeAnchorKey
		}
	}
	require.NoError(t, tapsend.PrepareOutputAssets(t.Context(), packet))
	witness := opTrueWitness(t)
	for _, output := range packet.Outputs {
		require.NoError(t, output.Asset.UpdateTxWitness(0, witness))
		if output.SplitAsset != nil {
			require.NoError(t, output.SplitAsset.UpdateTxWitness(
				0, witness,
			))
		}
	}

	commitments, err := tapsend.CreateOutputCommitments(
		packets, tapsend.WithSpenderLeaves(),
	)
	require.NoError(t, err)
	anchor, err := tapsend.PrepareAnchoringTemplate(packets)
	require.NoError(t, err)
	require.NoError(t, tapsend.UpdateTaprootOutputKeys(
		anchor, packet, commitments,
	))

	for idx, output := range packet.Outputs {
		if !output.ScriptKey.PubKey.IsEqual(htlcScriptKey.PubKey) {
			continue
		}

		htlcProof, err := tapsend.CreateProofSuffix(
			anchor.UnsignedTx, anchor.Outputs, packet, commitments,
			idx, packets,
		)
		require.NoError(t, err)
		_, err = htlcProof.VerifyProofs()
		require.NoError(t, err)

		return htlcProof
	}
	require.FailNow(t, "funding transfer has no HTLC output")

	return nil
}

// hideAnchorLeaves rebuilds the fixture's HTLC anchor so it also commits to
// the hidden assets, which the HTLC proof does not disclose. The HTLC proof
// stays structurally valid. The returned commitment proves the hidden leaves.
func hideAnchorLeaves(t *testing.T, f *witnessFixture,
	hidden ...*asset.Asset) *commitment.TapCommitment {

	t.Helper()

	version := commitment.TapCommitmentV2
	anchorCommitment, err := commitment.FromAssets(
		&version, append([]*asset.Asset{&f.proof.Asset}, hidden...)...,
	)
	require.NoError(t, err)
	sibling, err := f.kit.GetSiblingPreimage()
	require.NoError(t, err)
	siblingHash, err := sibling.TapHash()
	require.NoError(t, err)
	internalKey, err := f.kit.GetAggregateKey()
	require.NoError(t, err)
	script, err := tapscript.PayToAddrScript(
		*internalKey, siblingHash, *anchorCommitment,
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
		Proof: *inclusion, TapSiblingPreimage: &sibling,
	}
	_, err = f.proof.VerifyProofs()
	require.NoError(t, err)

	return anchorCommitment
}

// hiddenAsset returns an OP_TRUE asset of another genesis for the fixture's
// HTLC anchor.
func hiddenAsset(f *witnessFixture) *asset.Asset {
	hidden := f.proof.Asset.Copy()
	hidden.Genesis.FirstPrevOut = wire.OutPoint{Hash: chainhash.Hash{0x77}}
	hidden.Amount = 100

	return hidden
}

// encodeProofLeaves serializes the proof's asset and each alt leaf separately,
// which also works for alt leaves that cannot be encoded together.
func encodeProofLeaves(t *testing.T, p *proof.Proof) []byte {
	t.Helper()

	var encoded bytes.Buffer
	require.NoError(t, p.Asset.Encode(&encoded))
	for _, leaf := range asset.FromAltLeaves(p.AltLeaves) {
		require.NoError(t, leaf.EncodeAltLeaf(&encoded))
	}

	return encoded.Bytes()
}

// claimReconstruction rebuilds the HTLC input commitment from the proof alone,
// as the transfer validator does before signing a claim.
func claimReconstruction(t *testing.T, p *proof.Proof) error {
	t.Helper()

	proofCopy := *p
	proofCopy.Asset = *p.Asset.Copy()
	packet, err := tappsbt.FromProofs(
		[]*proof.Proof{&proofCopy}, &address.RegressionNetTap, tappsbt.V1,
	)
	require.NoError(t, err)
	input := packet.Inputs[0].Asset()
	if input.HasSplitCommitmentWitness() &&
		*input.PrevWitnesses[0].PrevID == asset.ZeroPrevID {

		input.PrevWitnesses[0].SplitCommitment = nil
	}

	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{PreviousOutPoint: p.OutPoint()})
	btc, err := psbt.NewFromUnsignedTx(tx)
	require.NoError(t, err)
	btc.Inputs[0].WitnessUtxo = p.AnchorTx.TxOut[p.InclusionProof.OutputIndex]
	btc.Inputs[0].TaprootInternalKey = schnorr.SerializePubKey(
		p.InclusionProof.InternalKey,
	)

	return tapsend.ValidateAnchorInputs(
		btc, []*tappsbt.VPacket{packet}, nil,
	)
}

// TestVerifyCompleteAnchorMatchesClaim requires acceptance to pass exactly when
// the receiver can rebuild the HTLC input commitment from the proof alone, as
// signing a claim requires. Honest proofs come from tapd's own coin
// distribution and proof generation; the others hide or alter anchor leaves.
func TestVerifyCompleteAnchorMatchesClaim(t *testing.T) {
	type testCase struct {
		name        string
		proof       func(*testing.T) *proof.Proof
		errContains string
	}
	fixtureProof := func(assetVersion asset.Version,
		commitmentVersion commitment.TapCommitmentVersion) func(
		*testing.T) *proof.Proof {

		return func(t *testing.T) *proof.Proof {
			return newWitnessFixtureWithVersions(
				t, SuccessSequence, assetVersion,
				commitmentVersion,
			).proof
		}
	}
	hiddenLeaf := func(hidden func(*testing.T,
		*witnessFixture) *asset.Asset) func(*testing.T) *proof.Proof {

		return func(t *testing.T) *proof.Proof {
			f := newWitnessFixture(t, SuccessSequence)
			hideAnchorLeaves(t, f, hidden(t, f))

			return f.proof
		}
	}
	rootProof := func(t *testing.T) *proof.Proof {
		f := newWitnessFixture(t, SuccessSequence)
		htlcProof := fundHtlcProof(t, f, fundingLayouts()[0])
		require.NotEmpty(t, htlcProof.AltLeaves)

		return htlcProof
	}

	tests := []testCase{
		{
			name:  "issuance leaf",
			proof: fixtureProof(asset.V1, commitment.TapCommitmentV2),
		},
		{
			name:  "legacy v0 commitment",
			proof: fixtureProof(asset.V0, commitment.TapCommitmentV0),
		},
		{
			name:  "legacy v1 commitment",
			proof: fixtureProof(asset.V1, commitment.TapCommitmentV1),
		},
		{
			name: "hidden asset",
			proof: hiddenLeaf(func(_ *testing.T,
				f *witnessFixture) *asset.Asset {

				return hiddenAsset(f)
			}),
			errContains: testIncompleteAnchor,
		},
		{
			name: "hidden asset with the same ID",
			proof: hiddenLeaf(func(t *testing.T,
				f *witnessFixture) *asset.Asset {

				_, key := scalarKey(t, 0x55)
				hidden := f.proof.Asset.Copy()
				hidden.ScriptKey = asset.NewScriptKeyBip86(
					keychain.KeyDescriptor{PubKey: key},
				)

				return hidden
			}),
			errContains: testIncompleteAnchor,
		},
		{
			name: "hidden tombstone",
			proof: hiddenLeaf(func(_ *testing.T,
				f *witnessFixture) *asset.Asset {

				tombstone := f.proof.Asset.Copy()
				tombstone.Amount = 0
				tombstone.ScriptKey = asset.NUMSScriptKey

				return tombstone
			}),
			errContains: testIncompleteAnchor,
		},
		{
			name: "dropped alt leaf",
			proof: func(t *testing.T) *proof.Proof {
				htlcProof := rootProof(t)
				htlcProof.AltLeaves = htlcProof.AltLeaves[1:]

				return htlcProof
			},
			errContains: testIncompleteAnchor,
		},
		{
			name: "extra alt leaf",
			proof: func(t *testing.T) *proof.Proof {
				htlcProof := rootProof(t)
				spent := htlcProof.Asset.Copy()
				spent.PrevWitnesses = []asset.Witness{{
					PrevID: &asset.PrevID{
						OutPoint: wire.OutPoint{
							Hash: chainhash.Hash{0x99},
						},
						ID: spent.ID(),
					},
				}}
				extra, err := asset.CollectSTXO(spent)
				require.NoError(t, err)
				htlcProof.AltLeaves = append(
					htlcProof.AltLeaves, extra...,
				)

				return htlcProof
			},
			errContains: testIncompleteAnchor,
		},
		{
			name: "duplicate alt leaf",
			proof: func(t *testing.T) *proof.Proof {
				htlcProof := rootProof(t)
				htlcProof.AltLeaves = append(
					htlcProof.AltLeaves, htlcProof.AltLeaves[0],
				)

				return htlcProof
			},
			errContains: "invalid asset proof alt leaves",
		},
	}
	for _, layout := range fundingLayouts() {
		tests = append(tests, testCase{
			name: layout.name,
			proof: func(t *testing.T) *proof.Proof {
				f := newWitnessFixture(t, SuccessSequence)

				return fundHtlcProof(t, f, layout)
			},
		})
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			htlcProof := test.proof(t)
			before := encodeProofLeaves(t, htlcProof)
			proven, acceptErr := VerifyCompleteAnchor(htlcProof)
			require.Equal(t, before, encodeProofLeaves(t, htlcProof))

			claimErr := claimReconstruction(t, htlcProof)
			require.Equal(
				t, acceptErr == nil, claimErr == nil,
				"acceptance: %v, claim: %v", acceptErr, claimErr,
			)
			if test.errContains == "" {
				require.NoError(t, acceptErr)
				expected, err := htlcProof.VerifyProofs()
				require.NoError(t, err)
				require.Equal(
					t, expected.TapscriptRoot(nil),
					proven.TapscriptRoot(nil),
				)

				return
			}

			// The proof itself stays valid, so only the incomplete
			// anchor description can cause the rejection.
			_, err := htlcProof.VerifyProofs()
			require.NoError(t, err)
			require.ErrorContains(t, acceptErr, test.errContains)
		})
	}
}

// TestVerifyCompleteAnchorRejectsInvalidProofs keeps the structural proof
// checks in front of the completeness check.
func TestVerifyCompleteAnchorRejectsInvalidProofs(t *testing.T) {
	_, err := VerifyCompleteAnchor(nil)
	require.ErrorContains(t, err, "asset proof is required")

	f := newWitnessFixture(t, SuccessSequence)
	f.proof.InclusionProof.InternalKey = nil
	_, err = VerifyCompleteAnchor(f.proof)
	require.ErrorContains(t, err, "invalid asset proof")
}

// TestGetPkScriptFromProofRequiresCompleteAnchor resolves every honest funding
// layout to its on-chain HTLC script and rejects anchors with hidden leaves.
func TestGetPkScriptFromProofRequiresCompleteAnchor(t *testing.T) {
	for _, layout := range fundingLayouts() {
		t.Run(layout.name, func(t *testing.T) {
			f := newWitnessFixture(t, SuccessSequence)
			htlcProof := fundHtlcProof(t, f, layout)
			pkScript, err := f.kit.GetPkScriptFromProof(htlcProof)
			require.NoError(t, err)
			output := htlcProof.AnchorTx.TxOut[htlcProof.InclusionProof.
				OutputIndex]
			require.Equal(t, output.PkScript, pkScript)
		})
	}

	t.Run("hidden leaf", func(t *testing.T) {
		f := newWitnessFixture(t, SuccessSequence)
		hideAnchorLeaves(t, f, hiddenAsset(f))
		_, err := f.kit.GetPkScriptFromProof(f.proof)
		require.ErrorContains(t, err, testIncompleteAnchor)
		_, err = GenTaprootAssetRootFromProof(f.proof)
		require.ErrorContains(t, err, testIncompleteAnchor)
	})
}
