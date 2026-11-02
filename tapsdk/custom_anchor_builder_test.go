package tapsdk

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/commitment"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/tappsbt"
	"github.com/lightninglabs/taproot-assets/tapsend"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// customAnchorBuilderTestClient embeds Client so each test only needs to
// implement the calls that are expected along the custom-anchor path. It does
// not implement CustomAnchorCapabilityProvider; that distinction is used by
// the fail-closed lifecycle test.
type customAnchorBuilderTestClient struct {
	Client

	verifyProof func(context.Context, []byte) (*VerifyProofResponse, error)
	signVirtual func(context.Context, []byte) ([]byte, error)
	commit      func(context.Context, *CommitVirtualPsbtsRequest) (
		*CommitVirtualPsbtsResponse, error)
	publish func(context.Context, *PublishAndLogTransferRequest) (
		*AssetPacket, error)
	queryScriptKey func(context.Context, []byte) (*ScriptKey, error)
}

func (c *customAnchorBuilderTestClient) QueryScriptKey(ctx context.Context,
	key []byte) (*ScriptKey, error) {

	if c.queryScriptKey == nil {
		panic("unexpected QueryScriptKey call")
	}

	return c.queryScriptKey(ctx, key)
}

func (c *customAnchorBuilderTestClient) VerifyProof(ctx context.Context,
	raw []byte) (*VerifyProofResponse, error) {

	if c.verifyProof == nil {
		panic("unexpected VerifyProof call")
	}

	return c.verifyProof(ctx, raw)
}

func (c *customAnchorBuilderTestClient) SignVirtualPsbt(ctx context.Context,
	packet []byte) ([]byte, error) {

	if c.signVirtual == nil {
		panic("unexpected SignVirtualPsbt call")
	}

	return c.signVirtual(ctx, packet)
}

func (c *customAnchorBuilderTestClient) CommitVirtualPsbtsWithRequest(
	ctx context.Context, req *CommitVirtualPsbtsRequest) (
	*CommitVirtualPsbtsResponse, error) {

	if c.commit == nil {
		panic("unexpected CommitVirtualPsbts call")
	}

	return c.commit(ctx, req)
}

func (c *customAnchorBuilderTestClient) PublishAndLogTransferWithRequest(
	ctx context.Context, req *PublishAndLogTransferRequest) (
	*AssetPacket, error) {

	if c.publish == nil {
		panic("unexpected PublishAndLogTransfer call")
	}

	return c.publish(ctx, req)
}

type capableCustomAnchorBuilderTestClient struct {
	*customAnchorBuilderTestClient

	capabilities  *CustomAnchorCapabilities
	capabilityErr error
}

func (c *capableCustomAnchorBuilderTestClient) CustomAnchorCapabilities(
	context.Context) (*CustomAnchorCapabilities, error) {

	if c.capabilityErr != nil {
		return nil, c.capabilityErr
	}
	if c.capabilities == nil {
		return nil, nil
	}

	clone := *c.capabilities
	return &clone, nil
}

type customAnchorBuilderFixture struct {
	request        *CustomAnchorRequest
	proofFile      []byte
	inputProof     *proof.Proof
	assetSpendKey  *btcec.PrivateKey
	anchorInputKey *btcec.PrivateKey
	client         *customAnchorBuilderTestClient
	btcInputKey    *btcec.PrivateKey
}

// TestCustomAnchorOutputCommitmentPreviewSupportsBackendSigning proves V1
// output roots do not depend on the delegated virtual-input signature.
func TestCustomAnchorOutputCommitmentPreviewSupportsBackendSigning(
	t *testing.T) {

	t.Parallel()

	fixture := newCustomAnchorBuilderFixture(t)
	var signCalls int
	fixture.client.signVirtual = func(_ context.Context, packet []byte) (
		[]byte, error) {

		signCalls++
		return customAnchorSignVirtualPacket(
			t, fixture.assetSpendKey, packet,
		), nil
	}
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
	plan, err := NewWallet(client, NetworkRegtest).
		NewCustomAnchorTxBuilder().Build(
		context.Background(), fixture.request,
	)
	require.NoError(t, err)

	previews, err := plan.PreviewOutputCommitments()
	require.NoError(t, err)
	require.Len(t, previews, 1)
	require.Zero(t, signCalls)

	sealed, err := plan.Commit(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, signCalls)
	require.Len(t, sealed.Outputs, 1)
	require.Equal(t, previews[0].TaprootAssetRoot,
		sealed.Outputs[0].TaprootAssetRoot)
	require.Equal(t, previews[0].TaprootMerkleRoot,
		sealed.Outputs[0].TaprootMerkleRoot)
}

// TestCustomAnchorOutputCommitmentPreviewRejectsEmptyPlan prevents a manually
// constructed zero-value plan from appearing to have a valid empty preview.
func TestCustomAnchorOutputCommitmentPreviewRejectsEmptyPlan(t *testing.T) {
	t.Parallel()

	_, err := (&CustomAnchorPlan{}).PreviewOutputCommitments()
	require.ErrorContains(t, err, "nil custom anchor plan")
}

func TestCustomAnchorBuilderRequiresWalletFundingLock(t *testing.T) {
	t.Parallel()

	fixture := newCustomAnchorBuilderFixture(t)
	feeRate, err := NewFeeRateSatPerVByte(2)
	require.NoError(t, err)
	fixture.request.Funding = CustomAnchorFundingPlan{
		Mode: CustomAnchorFundingWalletFunded,
		WalletFunded: &CustomAnchorWalletFunding{
			ChangeOutput: AnchorChangeOutput{
				Mode: AnchorChangeOutputAdd,
			},
			Fee: AnchorFee{
				Mode:    AnchorFeeSatPerVByte,
				FeeRate: feeRate,
			},
			MaxFeeSat: 10_000,
		},
	}

	_, err = NewWallet(fixture.client, NetworkRegtest).
		NewCustomAnchorTxBuilder().Build(
		context.Background(), fixture.request,
	)
	require.ErrorContains(
		t, err, "wallet-funded custom lock ID is required",
	)
}

func TestCustomAnchorBuilderRejectsInvalidP2AParentPolicy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*psbt.Packet)
		wantErr string
	}{
		{
			name: "positive fee",
			mutate: func(packet *psbt.Packet) {
				packet.Inputs[0].WitnessUtxo.Value++
			},
			wantErr: "must pay exactly zero fee",
		},
		{
			name: "additional dust output",
			mutate: func(packet *psbt.Packet) {
				delta := packet.UnsignedTx.TxOut[0].Value - 1
				packet.UnsignedTx.TxOut[0].Value = 1
				packet.Inputs[0].WitnessUtxo.Value -= delta
			},
			wantErr: "must be the only dust output",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			fixture := newCustomAnchorBuilderFixture(t)
			anchor := mustDecodeAnchorPSBT(t, fixture.request.AnchorPSBT)
			test.mutate(anchor)
			fixture.request.AnchorPSBT = mustSerializeAnchorPSBT(t, anchor)

			_, err := NewWallet(fixture.client, NetworkRegtest).
				NewCustomAnchorTxBuilder().Build(
				context.Background(), fixture.request,
			)
			require.ErrorContains(t, err, test.wantErr)
		})
	}
}

func TestCustomAnchorBuilderPreservesExactTemplate(t *testing.T) {
	t.Parallel()

	fixture := newCustomAnchorBuilderFixture(t)
	wallet := NewWallet(fixture.client, NetworkRegtest)
	builder := wallet.NewCustomAnchorTxBuilder()

	originalAnchor, err := decodeAnchorPSBT(fixture.request.AnchorPSBT)
	require.NoError(t, err)
	originalRequest := fixture.request.Clone()

	plan, err := builder.Build(context.Background(), fixture.request)
	require.NoError(t, err)
	secondPlan, err := builder.Build(context.Background(), originalRequest)
	require.NoError(t, err)

	preparedAnchor, err := decodeAnchorPSBT(plan.AnchorPSBT())
	require.NoError(t, err)
	require.NoError(t, compareUnsignedAnchorTransactions(
		originalAnchor.UnsignedTx, preparedAnchor.UnsignedTx,
	))
	require.Equal(t, int32(3), preparedAnchor.UnsignedTx.Version)
	require.Equal(t, uint32(42), preparedAnchor.UnsignedTx.LockTime)
	require.Equal(t, uint32(0x11111111),
		preparedAnchor.UnsignedTx.TxIn[0].Sequence)
	require.Equal(t, uint32(0x22222222),
		preparedAnchor.UnsignedTx.TxIn[1].Sequence)
	require.Equal(t, int64(12_345),
		preparedAnchor.UnsignedTx.TxOut[0].Value)
	require.Equal(t, []byte{txscript.OP_TRUE},
		preparedAnchor.UnsignedTx.TxOut[0].PkScript)
	require.Equal(t, int64(777),
		preparedAnchor.UnsignedTx.TxOut[1].Value)
	require.Equal(t, int64(0), preparedAnchor.UnsignedTx.TxOut[2].Value)
	require.Equal(t, canonicalP2AScript,
		preparedAnchor.UnsignedTx.TxOut[2].PkScript)

	firstID, err := plan.ID()
	require.NoError(t, err)
	secondID, err := secondPlan.ID()
	require.NoError(t, err)
	require.Equal(t, firstID, secondID)

	// Every accessor must return isolated storage. Mutating the source request
	// or any returned value must not change the plan or its deterministic ID.
	fixture.request.AnchorPSBT[0] ^= 0xff
	fixture.request.Inputs[0].ProofFile[0] ^= 0xff

	requestCopy := plan.Request()
	requestCopy.Inputs[0].ProofFile[0] ^= 0xff
	requestCopy.AnchorPSBT[0] ^= 0xff
	anchorCopy := plan.AnchorPSBT()
	anchorCopy[0] ^= 0xff
	activeCopy := plan.ActiveVirtualPSBTs()
	activeCopy[0][0] ^= 0xff
	verificationCopy := plan.Verification()
	verificationCopy.Checks[0].Message = "mutated"

	require.Equal(t, originalRequest.Inputs[0].ProofFile,
		plan.Request().Inputs[0].ProofFile)
	require.Equal(t, originalRequest.AnchorPSBT, plan.Request().AnchorPSBT)
	require.NotEqual(t, anchorCopy, plan.AnchorPSBT())
	require.NotEqual(t, activeCopy, plan.ActiveVirtualPSBTs())
	require.NotEqual(t, "mutated", plan.Verification().Checks[0].Message)

	afterMutationID, err := plan.ID()
	require.NoError(t, err)
	require.Equal(t, firstID, afterMutationID)
	finalVerification := plan.Verification()
	require.True(t, finalVerification.Valid())
}

func TestCustomAnchorBuilderOPTrueSpendInfo(t *testing.T) {
	t.Parallel()

	fixture := newCustomAnchorBuilderFixture(t)
	internalKey := testPrivateKey(t, 10)
	fixture.request.Outputs[0].Script = CustomAssetScriptPlan{
		Mode: CustomAssetScriptOPTrue,
		OPTrue: &CustomAssetOPTrueScriptPlan{
			InternalKey: KeyDescriptor{
				RawKeyBytes: mustCustomAnchorPubKey(
					t, internalKey.PubKey(),
				),
				KeyLocator: KeyLocator{Family: 212, Index: 7},
			},
		},
	}

	plan, err := NewWallet(fixture.client, NetworkRegtest).
		NewCustomAnchorTxBuilder().Build(
		context.Background(), fixture.request,
	)
	require.NoError(t, err)

	outputs := plan.Outputs()
	require.Len(t, outputs, 1)
	output := outputs[0]
	require.Equal(t, "asset-output", output.LogicalOutputID)
	require.NotNil(t, output.OPTrueSpend)
	require.Equal(t, []byte{txscript.OP_TRUE},
		output.OPTrueSpend.LeafScript)
	require.Equal(t, uint8(txscript.BaseLeafVersion),
		output.OPTrueSpend.LeafVersion)
	require.Equal(t, mustCustomAnchorPubKey(t, internalKey.PubKey()),
		output.OPTrueSpend.InternalKey)
	require.NoError(t, output.OPTrueSpend.Validate(output.ScriptKey))
	require.Equal(t, [][]byte{
		{txscript.OP_TRUE}, output.OPTrueSpend.ControlBlock,
	}, output.OPTrueSpend.WitnessStack())

	controlBlock, err := txscript.ParseControlBlock(
		output.OPTrueSpend.ControlBlock,
	)
	require.NoError(t, err)
	require.Len(t, controlBlock.InclusionProof, 32,
		"the sibling leaf makes the script key issuance-unique")

	// Plan accessors and witness construction must not expose internal
	// storage to the caller.
	outputs[0].OPTrueSpend.LeafScript[0] = txscript.OP_FALSE
	outputs[0].OPTrueSpend.ControlBlock[0] ^= 1
	witness := output.OPTrueSpend.WitnessStack()
	witness[0][0] = txscript.OP_FALSE
	witness[1][0] ^= 1

	again := plan.Outputs()[0]
	require.Equal(t, []byte{txscript.OP_TRUE},
		again.OPTrueSpend.LeafScript)
	require.NoError(t, again.OPTrueSpend.Validate(again.ScriptKey))
}

func TestCustomAnchorBuilderRejectsProofMismatches(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		mutateReq  func(*CustomAnchorRequest)
		mutateMock func(*customAnchorBuilderTestClient)
		wantErr    string
	}{
		{
			name: "local amount mismatch",
			mutateReq: func(req *CustomAnchorRequest) {
				req.Inputs[0].Amount = 99
				req.Outputs[0].Amount = 99
			},
			wantErr: "proof amount 100 does not match requested 99",
		},
		{
			name: "local asset identity mismatch",
			mutateReq: func(req *CustomAnchorRequest) {
				var otherID AssetID
				otherID[0] = 99
				otherRef := AssetRefFromAssetID(otherID)
				req.Inputs[0].AssetRef = otherRef
				req.Outputs[0].AssetRef = otherRef
			},
			wantErr: "proof asset ref",
		},
		{
			name: "backend verification error",
			mutateMock: func(client *customAnchorBuilderTestClient) {
				client.verifyProof = func(context.Context, []byte) (
					*VerifyProofResponse, error) {

					return nil, fmt.Errorf("backend unavailable")
				}
			},
			wantErr: "verify input 0 proof chain: backend unavailable",
		},
		{
			name: "backend invalid proof",
			mutateMock: func(client *customAnchorBuilderTestClient) {
				client.verifyProof = func(context.Context, []byte) (
					*VerifyProofResponse, error) {

					return &VerifyProofResponse{}, nil
				}
			},
			wantErr: "proof chain is not valid",
		},
		{
			name: "backend summary drift",
			mutateMock: func(client *customAnchorBuilderTestClient) {
				client.verifyProof = func(context.Context, []byte) (
					*VerifyProofResponse, error) {

					var wrongID AssetID
					wrongID[0] = 1
					return &VerifyProofResponse{
						Valid: true,
						DecodedProof: &DecodedProof{
							IssuanceID: wrongID,
							Amount:     100,
						},
					}, nil
				}
			},
			wantErr: "backend proof summary does not match",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			fixture := newCustomAnchorBuilderFixture(t)
			request := fixture.request.Clone()
			if test.mutateReq != nil {
				test.mutateReq(request)
			}
			if test.mutateMock != nil {
				test.mutateMock(fixture.client)
			}

			wallet := NewWallet(fixture.client, NetworkRegtest)
			_, err := wallet.NewCustomAnchorTxBuilder().Build(
				context.Background(), request,
			)
			require.ErrorContains(t, err, test.wantErr)
		})
	}
}

func TestCustomAnchorBuilderRejectsDuplicateConcreteInput(t *testing.T) {
	t.Parallel()

	fixture := newCustomAnchorBuilderFixture(t)
	request := fixture.request.Clone()
	duplicate := request.Inputs[0]
	duplicate.ID = "duplicate-prev-id"
	duplicate.ProofFile = cloneBytes(duplicate.ProofFile)
	request.Inputs = append(request.Inputs, duplicate)
	request.Outputs[0].Amount *= 2

	_, err := NewWallet(fixture.client, NetworkRegtest).
		NewCustomAnchorTxBuilder().Build(context.Background(), request)
	require.Error(t, err)
	require.ErrorContains(t, err, "duplicat")
}

func TestCustomAnchorBuilderGroupedMultiIssuance(t *testing.T) {
	t.Parallel()

	groupKey := &asset.GroupKey{
		GroupPubKey: *testPrivateKey(t, 21).PubKey(),
	}
	proofOne, first, spendOne := newCustomAnchorGroupedProof(
		t, 31, 40, groupKey,
	)
	proofTwo, second, spendTwo := newCustomAnchorGroupedProof(
		t, 32, 60, groupKey,
	)

	groupPubKey := mustCustomAnchorPubKey(t, &groupKey.GroupPubKey)
	groupRef := AssetRefFromGroupKey(groupPubKey)
	anchorKey := testPrivateKey(t, 40)
	assetOutputKey := customAnchorExternalScriptKey(
		t, testPrivateKey(t, 41),
	)

	anchor := customAnchorCallerTemplate(t, []*proof.Proof{first, second},
		2, 7, []uint32{0x01020304, 0x05060708, 0x090a0b0c},
		[]wire.TxOut{
			{Value: 500, PkScript: []byte{txscript.OP_TRUE}},
			{Value: 999, PkScript: []byte{txscript.OP_FALSE}},
		})
	request := &CustomAnchorRequest{
		Inputs: []CustomAssetInput{
			{
				ID:        "issuance-one",
				AssetRef:  groupRef,
				Amount:    40,
				ProofFile: proofOne,
				Witness: CustomAssetWitnessPlan{
					Mode: CustomAssetWitnessBackendSigner,
				},
			},
			{
				ID:        "issuance-two",
				AssetRef:  groupRef,
				Amount:    60,
				ProofFile: proofTwo,
				Witness: CustomAssetWitnessPlan{
					Mode: CustomAssetWitnessBackendSigner,
				},
			},
		},
		Outputs: []CustomAssetOutput{{
			ID:                "group-receiver",
			AssetRef:          groupRef,
			Amount:            100,
			AnchorOutputIndex: 1,
			AnchorValueSat:    999,
			Script: CustomAssetScriptPlan{
				Mode: CustomAssetScriptExternal,
				External: &CustomAssetExternalScriptPlan{
					ScriptKey: assetOutputKey,
				},
			},
			Anchor: CustomAnchorOutputPlan{
				InternalKey: InternalKey{
					PubKey: mustCustomAnchorPubKey(
						t, anchorKey.PubKey(),
					),
				},
			},
		}},
		AnchorPSBT: anchor,
		Funding: CustomAnchorFundingPlan{
			Mode:              CustomAnchorFundingCallerFundedExact,
			CallerFundedExact: &CustomAnchorCallerFundedExact{},
		},
		PassiveAssets: CustomAnchorPassiveAssets{
			Policy: CustomAnchorPassiveReject,
		},
		SigningPlans: []CustomAnchorInputSigningPlan{
			{
				InputIndex: 0,
				KeyPath: &CustomAnchorKeyPathSigningPlan{
					Signer: mustCustomAnchorXOnly(
						t, testPrivateKey(t, 7).PubKey(),
					),
				},
			},
			{
				InputIndex: 1,
				KeyPath: &CustomAnchorKeyPathSigningPlan{
					Signer: mustCustomAnchorXOnly(
						t, testPrivateKey(t, 81).PubKey(),
					),
				},
			},
			{
				InputIndex: 2,
				KeyPath: &CustomAnchorKeyPathSigningPlan{
					Signer: mustCustomAnchorXOnly(
						t, testPrivateKey(t, 82).PubKey(),
					),
				},
			},
		},
	}

	client := &customAnchorBuilderTestClient{
		verifyProof: customAnchorSuccessfulProofVerification,
		queryScriptKey: func(_ context.Context,
			key []byte) (*ScriptKey, error) {

			for _, privateKey := range []*btcec.PrivateKey{
				spendOne, spendTwo,
			} {
				resolved := customAnchorExternalScriptKey(
					t, privateKey,
				)
				if bytes.Equal(
					key, schnorr.SerializePubKey(
						mustParseCustomAnchorPubKey(
							t, resolved.PubKey,
						),
					),
				) {

					return &resolved, nil
				}
			}

			return nil, fmt.Errorf("unknown grouped script key")
		},
	}
	plan, err := NewWallet(client, NetworkRegtest).
		NewCustomAnchorTxBuilder().Build(context.Background(), request)
	require.NoError(t, err)
	require.Len(t, plan.ActiveVirtualPSBTs(), 2)

	issuances := make(map[asset.ID]struct{})
	scriptKeys := make(map[string]struct{})
	var outputTotal uint64
	for _, packetBytes := range plan.ActiveVirtualPSBTs() {
		packet, err := tappsbt.Decode(packetBytes)
		require.NoError(t, err)
		require.Len(t, packet.Inputs, 1)
		require.Len(t, packet.Outputs, 1)

		issuanceID, err := packet.AssetID()
		require.NoError(t, err)
		issuances[issuanceID] = struct{}{}
		output := packet.Outputs[0]
		require.Equal(t, uint32(1), output.AnchorOutputIndex)
		outputTotal += output.Amount
		scriptKeys[string(output.ScriptKey.PubKey.SerializeCompressed())] =
			struct{}{}
	}

	require.Len(t, issuances, 2)
	require.Len(t, scriptKeys, 2,
		"each concrete issuance needs a unique commitment key")
	require.Equal(t, uint64(100), outputTotal)

	client.signVirtual = func(_ context.Context, packetBytes []byte) (
		[]byte, error) {

		packet, err := tappsbt.Decode(packetBytes)
		if err != nil {
			return nil, err
		}
		issuanceID, err := packet.AssetID()
		if err != nil {
			return nil, err
		}
		switch issuanceID {
		case first.Asset.ID():
			return customAnchorSignVirtualPacket(
				t, spendOne, packetBytes,
			), nil

		case second.Asset.ID():
			return customAnchorSignVirtualPacket(
				t, spendTwo, packetBytes,
			), nil

		default:
			return nil, fmt.Errorf("unknown grouped issuance %x", issuanceID)
		}
	}
	client.commit = func(_ context.Context,
		req *CommitVirtualPsbtsRequest) (*CommitVirtualPsbtsResponse, error) {

		return customAnchorTestCommitResponse(t, req), nil
	}
	capabilities := DefaultTapdCustomAnchorCapabilities()
	capableClient := &capableCustomAnchorBuilderTestClient{
		customAnchorBuilderTestClient: client,
		capabilities:                  &capabilities,
	}
	committedPlan, err := NewWallet(capableClient, NetworkRegtest).
		NewCustomAnchorTxBuilder().Build(context.Background(), request)
	require.NoError(t, err)
	packageSnapshot, err := committedPlan.Commit(context.Background())
	require.NoError(t, err)
	require.NoError(t, packageSnapshot.Validate())
	require.Len(t, packageSnapshot.ActiveVirtualPsbts, 2)
	require.Len(t, packageSnapshot.Outputs, 2)
	require.Len(t, packageSnapshot.ProofUpdates, 2)
	packageIssuances := make(map[AssetID]struct{})
	for idx, output := range packageSnapshot.Outputs {
		packageIssuances[output.IssuanceID] = struct{}{}
		require.Equal(t, "group-receiver", output.LogicalOutputID)
		require.NotZero(t, output.TaprootAssetRoot)
		if idx > 0 {
			require.Equal(t,
				packageSnapshot.Outputs[0].TaprootAssetRoot,
				output.TaprootAssetRoot,
			)
			require.Equal(t,
				packageSnapshot.Outputs[0].TaprootMerkleRoot,
				output.TaprootMerkleRoot,
			)
		}
	}
	require.Len(t, packageIssuances, 2)

	// A concrete asset-ID ref can select one issuance from a fungible group
	// without silently expanding to the entire group.
	concreteRequest := request.Clone()
	concreteRef := AssetRefFromAssetID(AssetID(first.Asset.ID()))
	concreteRequest.Inputs = concreteRequest.Inputs[:1]
	concreteRequest.Inputs[0].AssetRef = concreteRef
	concreteRequest.Outputs[0].AssetRef = concreteRef
	concreteRequest.Outputs[0].Amount = 40
	concreteRequest.AnchorPSBT = customAnchorCallerTemplate(
		t, []*proof.Proof{first}, 2, 7,
		[]uint32{0x01020304, 0x05060708}, []wire.TxOut{
			{Value: 500, PkScript: []byte{txscript.OP_TRUE}},
			{Value: 999, PkScript: []byte{txscript.OP_FALSE}},
		},
	)
	concreteRequest.SigningPlans = concreteRequest.SigningPlans[:2]
	concretePlan, err := NewWallet(capableClient, NetworkRegtest).
		NewCustomAnchorTxBuilder().Build(
		context.Background(), concreteRequest,
	)
	require.NoError(t, err)
	require.Len(t, concretePlan.ActiveVirtualPSBTs(), 1)
	concretePackage, err := concretePlan.Commit(context.Background())
	require.NoError(t, err)
	require.NoError(t, concretePackage.Validate())
	require.Len(t, concretePackage.Inputs, 1)
	require.Equal(t, concreteRef, concretePackage.Inputs[0].AssetRef)

	// Allocations may cross issuance boundaries within one fungible group.
	// The two 40+60 inputs must map exactly to two logical 50+50 outputs.
	crossRequest := request.Clone()
	secondOutput := cloneCustomAssetOutput(crossRequest.Outputs[0])
	crossRequest.Outputs[0].Amount = 50
	secondOutput.ID = "group-receiver-two"
	secondOutput.Amount = 50
	secondOutput.AnchorOutputIndex = 2
	secondOutput.AnchorValueSat = 888
	secondOutput.Script.External.ScriptKey = customAnchorExternalScriptKey(
		t, testPrivateKey(t, 43),
	)
	secondOutput.Anchor.InternalKey.PubKey = mustCustomAnchorPubKey(
		t, testPrivateKey(t, 44).PubKey(),
	)
	crossRequest.Outputs = append(crossRequest.Outputs, secondOutput)
	crossRequest.AnchorPSBT = customAnchorCallerTemplate(
		t, []*proof.Proof{first, second}, 2, 7,
		[]uint32{0x01020304, 0x05060708, 0x090a0b0c},
		[]wire.TxOut{
			{Value: 500, PkScript: []byte{txscript.OP_TRUE}},
			{Value: 999, PkScript: []byte{txscript.OP_FALSE}},
			{Value: 888, PkScript: []byte{txscript.OP_FALSE}},
		},
	)
	crossPlan, err := NewWallet(client, NetworkRegtest).
		NewCustomAnchorTxBuilder().Build(
		context.Background(), crossRequest,
	)
	require.NoError(t, err)
	logicalTotals := make(map[string]uint64)
	for _, output := range crossPlan.Outputs() {
		logicalTotals[output.LogicalOutputID] += output.Amount
	}
	require.Equal(t, uint64(50), logicalTotals["group-receiver"])
	require.Equal(t, uint64(50), logicalTotals["group-receiver-two"])

	// OP_TRUE outputs need a different control block for each concrete
	// issuance because the hidden sibling leaf contains that issuance ID.
	opTrueRequest := request.Clone()
	opTrueInternalKey := testPrivateKey(t, 42)
	opTrueRequest.Outputs[0].Script = CustomAssetScriptPlan{
		Mode: CustomAssetScriptOPTrue,
		OPTrue: &CustomAssetOPTrueScriptPlan{
			InternalKey: KeyDescriptor{
				RawKeyBytes: mustCustomAnchorPubKey(
					t, opTrueInternalKey.PubKey(),
				),
			},
		},
	}
	opTruePlan, err := NewWallet(client, NetworkRegtest).
		NewCustomAnchorTxBuilder().Build(
		context.Background(), opTrueRequest,
	)
	require.NoError(t, err)

	plannedOutputs := opTruePlan.Outputs()
	require.Len(t, plannedOutputs, 2)
	controlBlocks := make(map[string]struct{}, len(plannedOutputs))
	for _, output := range plannedOutputs {
		require.NotNil(t, output.OPTrueSpend)
		require.NoError(t, output.OPTrueSpend.Validate(output.ScriptKey))
		controlBlocks[string(output.OPTrueSpend.ControlBlock)] = struct{}{}
	}
	require.Len(t, controlBlocks, 2)

	// Burn keys are derived from each concrete packet's first PrevID. They
	// must be protocol-recognized burns and remain unique across issuances.
	burnRequest := request.Clone()
	burnRequest.Outputs[0].Script = customAnchorBurnScript()
	burnRequest.LossPolicy = customAnchorLossPolicy(groupRef, 100)
	burnPlan, err := NewWallet(client, NetworkRegtest).
		NewCustomAnchorTxBuilder().Build(
		context.Background(), burnRequest,
	)
	require.NoError(t, err)
	burnKeys := make(map[string]struct{})
	for _, packetBytes := range burnPlan.ActiveVirtualPSBTs() {
		packet, err := tappsbt.Decode(packetBytes)
		require.NoError(t, err)
		require.Len(t, packet.Outputs, 1)
		require.True(t, packet.Outputs[0].Asset.IsBurn())
		burnKeys[string(
			packet.Outputs[0].ScriptKey.PubKey.SerializeCompressed(),
		)] = struct{}{}
	}
	require.Len(t, burnKeys, 2)
}

func TestCustomAnchorBuilderCreatesProtocolBurn(t *testing.T) {
	t.Parallel()

	fixture := newCustomAnchorBuilderFixture(t)
	fixture.request.Outputs[0].Script = customAnchorBurnScript()
	fixture.request.LossPolicy = customAnchorLossPolicy(
		fixture.request.Inputs[0].AssetRef,
		fixture.request.Inputs[0].Amount,
	)
	fixture.client.signVirtual = func(_ context.Context, packet []byte) (
		[]byte, error) {

		return customAnchorSignVirtualPacket(
			t, fixture.assetSpendKey, packet,
		), nil
	}
	fixture.client.commit = func(_ context.Context,
		req *CommitVirtualPsbtsRequest) (*CommitVirtualPsbtsResponse, error) {

		return customAnchorTestCommitResponse(t, req), nil
	}
	capabilities := DefaultTapdCustomAnchorCapabilities()
	client := &capableCustomAnchorBuilderTestClient{
		customAnchorBuilderTestClient: fixture.client,
		capabilities:                  &capabilities,
	}

	plan, err := NewWallet(client, NetworkRegtest).
		NewCustomAnchorTxBuilder().Build(
		context.Background(), fixture.request,
	)
	require.NoError(t, err)
	packet, err := tappsbt.Decode(plan.ActiveVirtualPSBTs()[0])
	require.NoError(t, err)
	require.Len(t, packet.Outputs, 1)
	require.True(t, packet.Outputs[0].Asset.IsBurn())

	packageSnapshot, err := plan.Commit(context.Background())
	require.NoError(t, err)
	require.NoError(t, packageSnapshot.Validate())
	require.Len(t, packageSnapshot.Outputs, 1)
	require.Equal(t, CustomAssetScriptBurn,
		packageSnapshot.Outputs[0].ScriptMode)
	committedPacket, err := tappsbt.Decode(
		packageSnapshot.ActiveVirtualPsbts[0],
	)
	require.NoError(t, err)
	require.True(t, committedPacket.Outputs[0].Asset.IsBurn())
}

func newCustomAnchorBuilderFixture(t *testing.T) *customAnchorBuilderFixture {
	t.Helper()

	proofFile, inputProof, assetSpendKey := newAssetProofPathBase(t)
	assetRef, _, _, err := assetIdentity(&inputProof.Asset)
	require.NoError(t, err)

	btcInputKey := testPrivateKey(t, 7)
	anchor := customAnchorCallerTemplate(
		t, []*proof.Proof{inputProof}, 3, 42,
		[]uint32{0x11111111, 0x22222222}, []wire.TxOut{
			{Value: 12_345, PkScript: []byte{txscript.OP_TRUE}},
			{Value: 777, PkScript: []byte{txscript.OP_FALSE}},
			{Value: 0, PkScript: cloneBytes(canonicalP2AScript)},
		},
	)

	outputInternalKey := testPrivateKey(t, 8)
	request := &CustomAnchorRequest{
		Inputs: []CustomAssetInput{{
			ID:        "asset-input",
			AssetRef:  assetRef,
			Amount:    inputProof.Asset.Amount,
			ProofFile: proofFile,
			Witness: CustomAssetWitnessPlan{
				Mode: CustomAssetWitnessBackendSigner,
			},
		}},
		Outputs: []CustomAssetOutput{{
			ID:                "asset-output",
			AssetRef:          assetRef,
			Amount:            inputProof.Asset.Amount,
			AnchorOutputIndex: 1,
			AnchorValueSat:    777,
			Script: CustomAssetScriptPlan{
				Mode: CustomAssetScriptExternal,
				External: &CustomAssetExternalScriptPlan{
					ScriptKey: customAnchorExternalScriptKey(
						t, testPrivateKey(t, 9),
					),
				},
			},
			Anchor: CustomAnchorOutputPlan{
				InternalKey: InternalKey{
					PubKey: mustCustomAnchorPubKey(
						t, outputInternalKey.PubKey(),
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
						t, inputProof.InclusionProof.InternalKey,
					),
				},
			},
		},
	}

	client := &customAnchorBuilderTestClient{
		verifyProof: customAnchorSuccessfulProofVerification,
		queryScriptKey: func(_ context.Context,
			key []byte) (*ScriptKey, error) {

			resolved := customAnchorExternalScriptKey(t, assetSpendKey)
			require.Equal(t, schnorr.SerializePubKey(
				inputProof.Asset.ScriptKey.PubKey,
			), key)
			return &resolved, nil
		},
	}

	return &customAnchorBuilderFixture{
		request:        request,
		proofFile:      proofFile,
		inputProof:     inputProof,
		assetSpendKey:  assetSpendKey,
		anchorInputKey: testPrivateKey(t, 2),
		client:         client,
		btcInputKey:    btcInputKey,
	}
}

func customAnchorSuccessfulProofVerification(_ context.Context, raw []byte) (
	*VerifyProofResponse, error) {

	file, err := proof.DecodeFile(raw)
	if err != nil {
		return nil, err
	}
	lastProof, err := file.LastProof()
	if err != nil {
		return nil, err
	}
	ref, issuanceID, _, err := assetIdentity(&lastProof.Asset)
	if err != nil {
		return nil, err
	}
	scriptKey, err := ParsePubKey(
		lastProof.Asset.ScriptKey.PubKey.SerializeCompressed(),
	)
	if err != nil {
		return nil, err
	}

	return &VerifyProofResponse{
		Valid: true,
		DecodedProof: &DecodedProof{
			AssetRef:   ref,
			IssuanceID: issuanceID,
			ScriptKey:  scriptKey,
			Amount:     lastProof.Asset.Amount,
			Outpoint:   outpointFromWire(lastProof.OutPoint()),
		},
	}, nil
}

func customAnchorCallerTemplate(t *testing.T, assetProofs []*proof.Proof,
	version int32, lockTime uint32, sequences []uint32,
	outputs []wire.TxOut) []byte {

	t.Helper()
	require.Len(t, sequences, len(assetProofs)+1)

	tx := wire.NewMsgTx(version)
	tx.LockTime = lockTime
	var btcHash chainhash.Hash
	btcHash[0] = 0xaa
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Hash: btcHash, Index: 17},
		Sequence:         sequences[0],
	})
	for idx := range assetProofs {
		tx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: assetProofs[idx].OutPoint(),
			Sequence:         sequences[idx+1],
		})
	}
	for idx := range outputs {
		output := outputs[idx]
		tx.AddTxOut(&wire.TxOut{
			Value:    output.Value,
			PkScript: cloneBytes(output.PkScript),
		})
	}

	packet, err := psbt.NewFromUnsignedTx(tx)
	require.NoError(t, err)
	btcInternalKey := testPrivateKey(t, 7).PubKey()
	btcOutputKey := txscript.ComputeTaprootKeyNoScript(btcInternalKey)
	btcPkScript, err := txscript.PayToTaprootScript(btcOutputKey)
	require.NoError(t, err)
	btcInputValue := int64(100_000)
	hasP2A := false
	var outputValue int64
	for _, output := range tx.TxOut {
		outputValue += output.Value
		if output.Value == 0 && bytes.Equal(
			output.PkScript, canonicalP2AScript,
		) {

			hasP2A = true
		}
	}
	if hasP2A {
		var assetInputValue int64
		for _, assetProof := range assetProofs {
			outpoint := assetProof.OutPoint()
			require.Equal(t, assetProof.AnchorTx.TxHash(), outpoint.Hash)
			require.Less(t, int(outpoint.Index), len(assetProof.AnchorTx.TxOut))
			assetInputValue += assetProof.AnchorTx.TxOut[outpoint.Index].Value
		}
		require.GreaterOrEqual(t, outputValue, assetInputValue)
		btcInputValue = outputValue - assetInputValue
	}
	packet.Inputs[0].WitnessUtxo = &wire.TxOut{
		Value:    btcInputValue,
		PkScript: btcPkScript,
	}
	packet.Inputs[0].TaprootInternalKey = schnorr.SerializePubKey(
		btcInternalKey,
	)

	encoded, err := serializePSBT(packet)
	require.NoError(t, err)
	return encoded
}

func customAnchorExternalScriptKey(t *testing.T,
	privateKey *btcec.PrivateKey) ScriptKey {

	t.Helper()
	rawKey := privateKey.PubKey()
	outputKey := txscript.ComputeTaprootKeyNoScript(rawKey)

	return ScriptKey{
		PubKey: mustCustomAnchorPubKey(t, outputKey),
		KeyDesc: KeyDescriptor{
			RawKeyBytes: mustCustomAnchorPubKey(t, rawKey),
		},
	}
}

func mustCustomAnchorPubKey(t *testing.T, key *btcec.PublicKey) PubKey {
	t.Helper()

	parsed, err := ParsePubKey(key.SerializeCompressed())
	require.NoError(t, err)
	return parsed
}

func mustParseCustomAnchorPubKey(t *testing.T, key PubKey) *btcec.PublicKey {
	t.Helper()

	parsed, err := btcec.ParsePubKey(key[:])
	require.NoError(t, err)
	return parsed
}

func mustCustomAnchorXOnly(t *testing.T, key *btcec.PublicKey) XOnlyPubKey {
	t.Helper()

	var result XOnlyPubKey
	copy(result[:], schnorr.SerializePubKey(key))
	return result
}

func newCustomAnchorGroupedProof(t *testing.T, seed byte, amount uint64,
	groupKey *asset.GroupKey) ([]byte, *proof.Proof, *btcec.PrivateKey) {

	t.Helper()
	spendKey := testPrivateKey(t, seed)
	internalKey := testPrivateKey(t, seed+50)
	var genesisHash chainhash.Hash
	genesisHash[0] = seed
	genesis := asset.Genesis{
		FirstPrevOut: wire.OutPoint{
			Hash:  genesisHash,
			Index: uint32(seed),
		},
		Tag:         fmt.Sprintf("grouped-%d", seed),
		OutputIndex: 0,
		Type:        asset.Normal,
	}
	scriptKey := asset.NewScriptKeyBip86(keychain.KeyDescriptor{
		PubKey: spendKey.PubKey(),
	})
	groupedAsset, err := asset.New(
		genesis, amount, 0, 0, scriptKey, groupKey,
		asset.WithAssetVersion(asset.V1),
	)
	require.NoError(t, err)
	commitmentVersion := commitment.TapCommitmentV2
	tapCommitment, err := commitment.FromAssets(
		&commitmentVersion, groupedAsset,
	)
	require.NoError(t, err)
	_, commitmentProof, err := tapCommitment.Proof(
		groupedAsset.TapCommitmentKey(),
		groupedAsset.AssetCommitmentKey(),
	)
	require.NoError(t, err)
	pkScript, _, _, err := tapsend.AnchorOutputScript(
		internalKey.PubKey(), nil, tapCommitment,
	)
	require.NoError(t, err)
	anchorTx := wire.NewMsgTx(2)
	anchorTx.AddTxIn(&wire.TxIn{PreviousOutPoint: genesis.FirstPrevOut})
	anchorTx.AddTxOut(&wire.TxOut{Value: 1_000, PkScript: pkScript})

	assetProof := &proof.Proof{
		PrevOut:  genesis.FirstPrevOut,
		AnchorTx: *anchorTx,
		Asset:    *groupedAsset,
		InclusionProof: proof.TaprootProof{
			OutputIndex: 0,
			InternalKey: internalKey.PubKey(),
			CommitmentProof: &proof.CommitmentProof{
				Proof: *commitmentProof,
			},
		},
		GenesisReveal: &genesis,
	}
	encoded, err := proof.EncodeAsProofFile(assetProof)
	require.NoError(t, err)
	decoded, err := proof.DecodeFile(encoded)
	require.NoError(t, err)
	lastProof, err := decoded.LastProof()
	require.NoError(t, err)
	require.True(t, bytes.Equal(
		lastProof.Asset.GroupKey.GroupPubKey.SerializeCompressed(),
		groupKey.GroupPubKey.SerializeCompressed(),
	))

	return encoded, assetProof, spendKey
}
