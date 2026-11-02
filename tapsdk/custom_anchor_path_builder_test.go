package tapsdk

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/tappsbt"
	"github.com/lightninglabs/taproot-assets/tapsend"
	"github.com/stretchr/testify/require"
)

// TestCustomAnchorPathBuilderConsumesUnconfirmedTip verifies the complete Ark
// edge case: the confirmed base is delegated to an injected verifier, the
// unconfirmed step is verified locally despite its BTC-only connector input,
// and the path tip becomes the virtual transaction predecessor.
func TestCustomAnchorPathBuilderConsumesUnconfirmedTip(t *testing.T) {
	t.Parallel()

	fixture := newCustomAnchorPathBuilderFixture(t)
	builder := NewWallet(fixture.client, NetworkRegtest).
		NewCustomAnchorTxBuilder()
	builder.SetConfirmedProofVerifier(fixture.verifier)

	plan, err := builder.Build(
		context.Background(), fixture.request,
	)
	require.NoError(t, err)
	require.Equal(t, 1, fixture.verifier.calls)
	require.Len(t, fixture.transition.AnchorTx.TxIn, 2,
		"the path step should retain its BTC-only connector input")

	var baseCheck, hostCheck, localCheck bool
	for _, check := range plan.Verification().Checks {
		switch check.Code {
		case customAnchorCheckProofChain:
			baseCheck = check.Origin == CustomAnchorVerificationOriginHost

		case customAnchorCheckBTCPath:
			hostCheck = check.Origin == CustomAnchorVerificationOriginHost

		case customAnchorCheckAssetPath:
			localCheck = check.Origin == CustomAnchorVerificationOriginLocal
		}
	}
	require.True(t, baseCheck,
		"the confirmed proof base must retain injected verifier trust")
	require.True(t, hostCheck,
		"the injected Bitcoin graph attestation must be host-trusted")
	require.True(t, localCheck,
		"the asset transition verification must remain SDK-local")

	packets := plan.ActiveVirtualPSBTs()
	require.Len(t, packets, 1)
	packet, err := tappsbt.Decode(packets[0])
	require.NoError(t, err)
	require.Len(t, packet.Inputs, 1)
	require.Equal(t, fixture.transition.OutPoint(),
		packet.Inputs[0].PrevID.OutPoint)
	require.Equal(t, fixture.transition.Asset.ID(),
		packet.Inputs[0].PrevID.ID)
	require.Equal(t, asset.ToSerialized(
		fixture.transition.Asset.ScriptKey.PubKey,
	), packet.Inputs[0].PrevID.ScriptKey)
	require.NotNil(t, packet.Inputs[0].Proof)
	require.Equal(t, fixture.transition.OutPoint(),
		packet.Inputs[0].Proof.OutPoint())
}

// TestCustomAnchorOutputCommitmentPreviewMatchesCommit proves a
// caller-witnessed plan can expose its exact output roots before the backend
// commits the transition.
func TestCustomAnchorOutputCommitmentPreviewMatchesCommit(t *testing.T) {
	t.Parallel()

	fixture := newCustomAnchorPathBuilderFixture(t)
	request := fixture.request.Clone()
	anchor, err := decodeAnchorPSBT(request.AnchorPSBT)
	require.NoError(t, err)
	equalCarrier := (anchor.UnsignedTx.TxOut[0].Value +
		anchor.UnsignedTx.TxOut[1].Value) / 2
	require.Equal(t, int64(6_561), equalCarrier)
	anchor.UnsignedTx.TxOut[0].Value = equalCarrier
	anchor.UnsignedTx.TxOut[1].Value = equalCarrier
	request.AnchorPSBT, err = serializePSBT(anchor)
	require.NoError(t, err)
	receiver := cloneCustomAssetOutput(request.Outputs[0])
	receiver.ID = "asset-receiver"
	receiver.Amount = 60
	receiver.AnchorValueSat = uint64(equalCarrier)
	change := cloneCustomAssetOutput(request.Outputs[0])
	change.ID = "asset-change"
	change.Amount = 40
	change.AnchorOutputIndex = 0
	change.AnchorValueSat = uint64(equalCarrier)
	change.Script.External.ScriptKey = customAnchorExternalScriptKey(
		t, testPrivateKey(t, 56),
	)
	change.Anchor.InternalKey.PubKey = mustCustomAnchorPubKey(
		t, testPrivateKey(t, 57).PubKey(),
	)
	request.Outputs = []CustomAssetOutput{receiver, change}
	witnessBuilder := NewWallet(fixture.client, NetworkRegtest).
		NewCustomAnchorTxBuilder()
	request.Inputs[0].Witness.Stack = customAnchorPathSpendWitness(
		t, witnessBuilder, fixture.transition, request.Outputs,
		fixture.tipSpendKey,
	)
	fixture.client.commit = func(_ context.Context,
		req *CommitVirtualPsbtsRequest) (*CommitVirtualPsbtsResponse,
		error) {

		return customAnchorTestCommitResponse(t, req), nil
	}
	capabilities := DefaultTapdCustomAnchorCapabilities()
	client := &capableCustomAnchorBuilderTestClient{
		customAnchorBuilderTestClient: fixture.client,
		capabilities:                  &capabilities,
	}
	builder := NewWallet(client, NetworkRegtest).
		NewCustomAnchorTxBuilder().SetConfirmedProofVerifier(
		fixture.verifier,
	)

	plan, err := builder.Build(context.Background(), request)
	require.NoError(t, err)
	beforePackets := plan.ActiveVirtualPSBTs()
	require.Len(t, beforePackets, 1)
	preparedPacket, err := tappsbt.Decode(beforePackets[0])
	require.NoError(t, err)
	isSplit, err := preparedPacket.HasSplitCommitment()
	require.NoError(t, err)
	require.True(t, isSplit)
	_, err = preparedPacket.SplitRootOutput()
	require.NoError(t, err)
	previews, err := plan.PreviewOutputCommitments()
	require.NoError(t, err)
	require.Len(t, previews, 2)
	require.Equal(t, uint32(1), previews[0].AnchorOutputIndex)
	require.Equal(t, uint32(0), previews[1].AnchorOutputIndex)
	for idx := range previews {
		require.NotZero(t, previews[idx].TaprootAssetRoot)
		require.NotZero(t, previews[idx].TaprootMerkleRoot)
	}
	repeated, err := plan.PreviewOutputCommitments()
	require.NoError(t, err)
	require.Equal(t, previews, repeated)
	require.Equal(t, beforePackets, plan.ActiveVirtualPSBTs())

	// Split locators bind the anchor output index. Rebuilding the same
	// equal-value logical allocations with swapped indices must therefore
	// produce different previews before a host checks for a stable canonical
	// permutation.
	swapped := request.Clone()
	swapped.Outputs[0].AnchorOutputIndex = 0
	swapped.Outputs[1].AnchorOutputIndex = 1
	swapped.Inputs[0].Witness.Stack = customAnchorPathSpendWitness(
		t, witnessBuilder, fixture.transition, swapped.Outputs,
		fixture.tipSpendKey,
	)
	swappedPlan, err := builder.Build(context.Background(), swapped)
	require.NoError(t, err)
	swappedPreviews, err := swappedPlan.PreviewOutputCommitments()
	require.NoError(t, err)
	require.Len(t, swappedPreviews, 2)
	require.Equal(t, previews[0].LogicalOutputID,
		swappedPreviews[0].LogicalOutputID)
	require.Equal(t, previews[1].LogicalOutputID,
		swappedPreviews[1].LogicalOutputID)
	require.NotEqual(t, previews, swappedPreviews)
	require.NotEqual(t, previews[0].TaprootAssetRoot,
		swappedPreviews[0].TaprootAssetRoot)

	sealed, err := plan.Commit(context.Background())
	require.NoError(t, err)
	require.Len(t, sealed.Outputs, 2)
	require.Len(t, sealed.ActiveVirtualPsbts, 1)
	committedPacket, err := tappsbt.Decode(sealed.ActiveVirtualPsbts[0])
	require.NoError(t, err)
	var alternateLeaves int
	for idx := range committedPacket.Outputs {
		alternateLeaves += len(committedPacket.Outputs[idx].AltLeaves)
	}
	require.Positive(t, alternateLeaves)
	for idx := range sealed.Outputs {
		require.Equal(t, sealed.Outputs[idx].LogicalOutputID,
			previews[idx].LogicalOutputID)
		require.Equal(t, sealed.Outputs[idx].LogicalOutputIndex,
			previews[idx].LogicalOutputIndex)
		require.Equal(t, sealed.Outputs[idx].AnchorOutputIndex,
			previews[idx].AnchorOutputIndex)
		require.Equal(t, sealed.Outputs[idx].TaprootAssetRoot,
			previews[idx].TaprootAssetRoot)
		require.Equal(t, sealed.Outputs[idx].TaprootMerkleRoot,
			previews[idx].TaprootMerkleRoot)
	}
}

// TestCustomAnchorPathBuilderRequiresOneProofSource keeps confirmed proofs and
// compact paths a disjoint input union.
func TestCustomAnchorPathBuilderRequiresOneProofSource(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*CustomAssetInput, *AssetProofPath)
	}{
		{
			name: "neither source",
			mutate: func(input *CustomAssetInput, _ *AssetProofPath) {
				input.ProofFile = nil
				input.ProofPath = nil
			},
		},
		{
			name: "both sources",
			mutate: func(input *CustomAssetInput,
				path *AssetProofPath) {

				input.ProofFile = cloneBytes(path.ConfirmedBaseProof)
				input.ProofPath = path.Clone()
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			fixture := newCustomAnchorPathBuilderFixture(t)
			request := fixture.request.Clone()
			test.mutate(&request.Inputs[0], fixture.path)
			builder := NewWallet(fixture.client, NetworkRegtest).
				NewCustomAnchorTxBuilder()
			builder.SetConfirmedProofVerifier(fixture.verifier)

			_, err := builder.Build(context.Background(), request)
			require.ErrorContains(t, err, "exactly one")
		})
	}
}

// TestCustomAnchorPathBuilderRequiresVerifier proves a compact path cannot
// silently downgrade the confirmed base to skip-chain verification.
func TestCustomAnchorPathBuilderRequiresVerifier(t *testing.T) {
	t.Parallel()

	fixture := newCustomAnchorPathBuilderFixture(t)
	builder := NewWallet(fixture.client, NetworkRegtest).
		NewCustomAnchorTxBuilder()

	_, err := builder.Build(context.Background(), fixture.request)
	require.ErrorContains(t, err, "confirmed proof verifier is required")
}

// TestCustomAnchorPathBuilderRejectsBackendSigner ensures tapd is never asked
// to sign an input state it has not imported from a confirmed proof file.
func TestCustomAnchorPathBuilderRejectsBackendSigner(t *testing.T) {
	t.Parallel()

	fixture := newCustomAnchorPathBuilderFixture(t)
	request := fixture.request.Clone()
	request.Inputs[0].Witness = CustomAssetWitnessPlan{
		Mode: CustomAssetWitnessBackendSigner,
	}
	builder := NewWallet(fixture.client, NetworkRegtest).
		NewCustomAnchorTxBuilder()
	builder.SetConfirmedProofVerifier(fixture.verifier)

	_, err := builder.Build(context.Background(), request)
	require.ErrorContains(t, err,
		"proof path inputs require caller-provided witnesses")
}

// TestCustomAnchorPathBuilderRejectsTipMismatches verifies the request binds
// the locally derived path-tip identity and exact amount.
func TestCustomAnchorPathBuilderRejectsTipMismatches(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*CustomAnchorRequest)
		wantErr string
	}{
		{
			name: "asset identity",
			mutate: func(request *CustomAnchorRequest) {
				var otherID AssetID
				otherID[0] = 99
				otherRef := AssetRefFromAssetID(otherID)
				request.Inputs[0].AssetRef = otherRef
				request.Outputs[0].AssetRef = otherRef
			},
			wantErr: "path asset ref",
		},
		{
			name: "amount",
			mutate: func(request *CustomAnchorRequest) {
				request.Inputs[0].Amount = 99
				request.Outputs[0].Amount = 99
			},
			wantErr: "path amount 100 does not match requested 99",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			fixture := newCustomAnchorPathBuilderFixture(t)
			request := fixture.request.Clone()
			test.mutate(request)
			builder := NewWallet(fixture.client, NetworkRegtest).
				NewCustomAnchorTxBuilder()
			builder.SetConfirmedProofVerifier(fixture.verifier)

			_, err := builder.Build(context.Background(), request)
			require.ErrorContains(t, err, test.wantErr)
		})
	}
}

// TestCustomAnchorPathBuilderRejectsDuplicateTip prevents counting the same
// concrete unconfirmed state twice even when logical input IDs differ.
func TestCustomAnchorPathBuilderRejectsDuplicateTip(t *testing.T) {
	t.Parallel()

	fixture := newCustomAnchorPathBuilderFixture(t)
	request := fixture.request.Clone()
	duplicate := request.Inputs[0]
	duplicate.ID = "duplicate-path-tip"
	duplicate.ProofPath = duplicate.ProofPath.Clone()
	request.Inputs = append(request.Inputs, duplicate)
	request.Outputs[0].Amount *= 2
	builder := NewWallet(fixture.client, NetworkRegtest).
		NewCustomAnchorTxBuilder()
	builder.SetConfirmedProofVerifier(fixture.verifier)

	_, err := builder.Build(context.Background(), request)
	require.ErrorContains(t, err, "duplicates asset predecessor")
}

type customAnchorPathBuilderFixture struct {
	request     *CustomAnchorRequest
	path        *AssetProofPath
	transition  *proof.Proof
	tipSpendKey *btcec.PrivateKey
	verifier    *testConfirmedProofVerifier
	client      *customAnchorBuilderTestClient
}

func newCustomAnchorPathBuilderFixture(
	t *testing.T) *customAnchorPathBuilderFixture {

	t.Helper()

	baseProofFile, baseProof, baseSpendKey := newAssetProofPathBase(t)
	tipSpendKey := testPrivateKey(t, 53)
	transition := newAssetProofPathTransition(
		t, baseProof, baseSpendKey, tipSpendKey,
	)
	transitionBytes, err := transition.Bytes()
	require.NoError(t, err)
	transitionBytes = mutateAssetProofPathTransition(
		t, transitionBytes, func(p *proof.Proof) {
			var connectorHash chainhash.Hash
			connectorHash[0] = 0x44
			p.AnchorTx.TxIn = append(p.AnchorTx.TxIn, &wire.TxIn{
				PreviousOutPoint: wire.OutPoint{
					Hash:  connectorHash,
					Index: 3,
				},
				Sequence: 0xfffffffd,
			})
		},
	)
	transition, err = proof.Decode(transitionBytes)
	require.NoError(t, err)

	path := &AssetProofPath{
		ConfirmedBaseProof: baseProofFile,
		Steps: []AssetProofPathStep{{
			TransitionProof: transitionBytes,
		}},
	}
	assetRef, _, _, err := assetIdentity(&transition.Asset)
	require.NoError(t, err)

	btcInputKey := testPrivateKey(t, 7)
	anchor := customAnchorCallerTemplate(
		t, []*proof.Proof{transition}, 3, 42,
		[]uint32{0x11111111, 0x22222222}, []wire.TxOut{
			{Value: 12_345, PkScript: []byte{txscript.OP_TRUE}},
			{Value: 777, PkScript: []byte{txscript.OP_FALSE}},
			{Value: 0, PkScript: cloneBytes(canonicalP2AScript)},
		},
	)
	request := &CustomAnchorRequest{
		Inputs: []CustomAssetInput{{
			ID:        "unconfirmed-path-tip",
			AssetRef:  assetRef,
			Amount:    transition.Asset.Amount,
			ProofPath: path,
		}},
		Outputs: []CustomAssetOutput{{
			ID:                "next-ark-edge",
			AssetRef:          assetRef,
			Amount:            transition.Asset.Amount,
			AnchorOutputIndex: 1,
			AnchorValueSat:    777,
			Script: CustomAssetScriptPlan{
				Mode: CustomAssetScriptExternal,
				External: &CustomAssetExternalScriptPlan{
					ScriptKey: customAnchorExternalScriptKey(
						t, testPrivateKey(t, 54),
					),
				},
			},
			Anchor: CustomAnchorOutputPlan{
				InternalKey: InternalKey{
					PubKey: mustCustomAnchorPubKey(
						t, testPrivateKey(t, 55).PubKey(),
					),
				},
			},
		}},
		AnchorPSBT: anchor,
		Funding: CustomAnchorFundingPlan{
			Mode: CustomAnchorFundingExternalP2AFeeBump,
			ExternalP2AFeeBump: &CustomAnchorExternalP2AFeeBump{
				P2AOutputIndex: 2,
			},
		},
		PassiveAssets: CustomAnchorPassiveAssets{
			Policy: CustomAnchorPassiveReject,
		},
		SigningPlans: []CustomAnchorInputSigningPlan{
			{
				InputIndex: 0,
				KeyPath: &CustomAnchorKeyPathSigningPlan{
					Signer: mustCustomAnchorXOnly(
						t, btcInputKey.PubKey(),
					),
				},
			},
			{
				InputIndex: 1,
				KeyPath: &CustomAnchorKeyPathSigningPlan{
					Signer: mustCustomAnchorXOnly(
						t,
						transition.InclusionProof.InternalKey,
					),
				},
			},
		},
	}

	client := &customAnchorBuilderTestClient{}
	builder := NewWallet(client, NetworkRegtest).
		NewCustomAnchorTxBuilder()
	request.Inputs[0].Witness = CustomAssetWitnessPlan{
		Mode: CustomAssetWitnessCallerProvided,
		Stack: customAnchorPathSpendWitness(
			t, builder, transition, request.Outputs, tipSpendKey,
		),
	}

	return &customAnchorPathBuilderFixture{
		request:     request,
		path:        path,
		transition:  transition,
		tipSpendKey: tipSpendKey,
		verifier: &testConfirmedProofVerifier{
			result: &ConfirmedProofVerification{
				AnchorAssetInventoryComplete: true,
			},
		},
		client: client,
	}
}

func customAnchorPathSpendWitness(t *testing.T,
	builder *CustomAnchorTxBuilder, inputProof *proof.Proof,
	outputs []CustomAssetOutput, spendKey *btcec.PrivateKey) [][]byte {

	t.Helper()

	allocations := make([]*tapsend.Allocation, len(outputs))
	for idx := range outputs {
		output := outputs[idx]
		internalKey := mustParseCustomAnchorPubKey(
			t, output.Anchor.InternalKey.PubKey,
		)
		scriptKey, err := tapAssetScriptKey(
			output.Script.External.ScriptKey,
		)
		require.NoError(t, err)
		allocations[idx] = &tapsend.Allocation{
			Type:         tapsend.CommitAllocationToRemote,
			OutputIndex:  output.AnchorOutputIndex,
			InternalKey:  internalKey,
			GenScriptKey: tapsend.StaticScriptKeyGen(scriptKey),
			Amount:       output.Amount,
			AssetVersion: asset.V1,
			BtcAmount:    btcutil.Amount(output.AnchorValueSat),
		}
	}
	packets, err := tapsend.DistributeCoins(
		[]*proof.Proof{inputProof}, allocations, builder.params, true,
		tappsbt.V1,
	)
	require.NoError(t, err)
	require.Len(t, packets, 1)
	require.NoError(t, tapsend.PrepareOutputAssets(
		context.Background(), packets[0],
	))
	encoded, err := tappsbt.Encode(packets[0])
	require.NoError(t, err)

	signed := customAnchorSignVirtualPacket(t, spendKey, encoded)
	signedPacket, err := tappsbt.Decode(signed)
	require.NoError(t, err)
	newAsset := signedPacket.Outputs[0].Asset
	isSplit, err := signedPacket.HasSplitCommitment()
	require.NoError(t, err)
	if isSplit {
		rootOutput, err := signedPacket.SplitRootOutput()
		require.NoError(t, err)
		newAsset = rootOutput.Asset
	}
	require.NotNil(t, newAsset)
	require.Len(t, newAsset.PrevWitnesses, 1)
	require.NotEmpty(t, newAsset.PrevWitnesses[0].TxWitness)

	return cloneWitness(newAsset.PrevWitnesses[0].TxWitness)
}
