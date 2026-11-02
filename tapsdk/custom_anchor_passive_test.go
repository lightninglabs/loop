package tapsdk

import (
	"context"
	"net/url"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/taproot-assets/address"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/commitment"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/tappsbt"
	"github.com/lightninglabs/taproot-assets/tapsend"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

const (
	// These are reserved fake endpoints, not credentials.
	//nolint:gosec
	customAnchorPassiveCourier = "universerpc://" +
		"proof.example:10029"
	//nolint:gosec
	customAnchorPassiveOtherCourier = "universerpc://" +
		"other.example:10029"
)

type customAnchorPassiveFixture struct {
	request          *CustomAnchorRequest
	client           *customAnchorBuilderTestClient
	activeProofFile  []byte
	passiveProofFile []byte
	activeProof      *proof.Proof
	passiveProof     *proof.Proof
	activeSpendKey   *btcec.PrivateKey
	passiveSpendKey  *btcec.PrivateKey
}

func TestCustomAnchorPassiveCallerReanchor(t *testing.T) {
	t.Parallel()

	fixture := newCustomAnchorPassiveFixture(t)
	fixture.client.signVirtual = func(_ context.Context, packet []byte) (
		[]byte, error) {

		return customAnchorSignVirtualPacket(
			t, fixture.activeSpendKey, packet,
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
	require.Len(t, plan.ActiveVirtualPSBTs(), 1)
	require.Len(t, plan.PassiveVirtualPSBTs(), 1)

	activePacket, err := tappsbt.Decode(plan.ActiveVirtualPSBTs()[0])
	require.NoError(t, err)
	passivePacket, err := tappsbt.Decode(plan.PassiveVirtualPSBTs()[0])
	require.NoError(t, err)
	require.Equal(t, activePacket.Inputs[0].PrevID.OutPoint,
		passivePacket.Inputs[0].PrevID.OutPoint)
	require.True(t, samePassiveAnchorOutput(
		activePacket.Outputs[0], passivePacket.Outputs[0],
	))
	require.Equal(t, customAnchorPassiveCourier,
		passivePacket.Outputs[0].ProofDeliveryAddress.String())

	inputs := plan.Inputs()
	require.Len(t, inputs, 2)
	require.Equal(t, CustomAnchorPacketRoleActive, inputs[0].PacketRole)
	require.Equal(t, CustomAnchorPacketRolePassive, inputs[1].PacketRole)
	require.Equal(t, "passive-asset", inputs[1].LogicalInputID)
	outputs := plan.Outputs()
	require.Len(t, outputs, 2)
	require.Equal(t, CustomAnchorPacketRolePassive, outputs[1].PacketRole)
	require.Equal(t, customAnchorPassiveCourier,
		outputs[1].ProofDelivery.CourierAddress)

	packageSnapshot, err := plan.Commit(context.Background())
	require.NoError(t, err)
	require.NoError(t, packageSnapshot.Validate())
	require.Len(t, packageSnapshot.ActiveVirtualPsbts, 1)
	require.Len(t, packageSnapshot.PassiveVirtualPsbts, 1)
	require.Len(t, packageSnapshot.Inputs, 2)
	require.Len(t, packageSnapshot.Outputs, 2)
	require.Len(t, packageSnapshot.ProofUpdates, 2)
	require.Equal(t, CustomAnchorPacketRolePassive,
		packageSnapshot.Inputs[1].PacketRole)
	require.Equal(t, CustomAnchorPacketRolePassive,
		packageSnapshot.Outputs[1].PacketRole)
	require.Equal(t, customAnchorPassiveCourier,
		packageSnapshot.Outputs[1].ProofDelivery.CourierAddress)
	require.Equal(t, packageSnapshot.Outputs[0].AnchorOutputIndex,
		packageSnapshot.Outputs[1].AnchorOutputIndex)
	require.Equal(t, packageSnapshot.Outputs[0].TaprootAssetRoot,
		packageSnapshot.Outputs[1].TaprootAssetRoot)
	require.Equal(t, packageSnapshot.Outputs[0].TaprootMerkleRoot,
		packageSnapshot.Outputs[1].TaprootMerkleRoot)
}

func TestCustomAnchorPassiveCallerReanchorRejectsMutation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*testing.T, *customAnchorPassiveFixture)
		wantErr string
	}{
		{
			name: "wrong proof",
			mutate: func(_ *testing.T,
				fixture *customAnchorPassiveFixture) {

				fixture.request.PassiveAssets.Packets[0].ProofFile =
					cloneBytes(fixture.activeProofFile)
			},
			wantErr: "does not match its proof tip",
		},
		{
			name: "wrong asset ref",
			mutate: func(t *testing.T,
				fixture *customAnchorPassiveFixture) {

				activeRef, _, _, err := assetIdentity(
					&fixture.activeProof.Asset,
				)
				require.NoError(t, err)
				fixture.request.PassiveAssets.Packets[0].AssetRef =
					activeRef
			},
			wantErr: "asset ref does not match",
		},
		{
			name: "wrong amount",
			mutate: func(_ *testing.T,
				fixture *customAnchorPassiveFixture) {

				fixture.request.PassiveAssets.Packets[0].Amount--
			},
			wantErr: "amount does not match requested",
		},
		{
			name: "input asset differs from proof",
			mutate: func(t *testing.T,
				fixture *customAnchorPassiveFixture) {

				mutateCustomAnchorPassiveVPacket(
					t, fixture.request, func(packet *tappsbt.VPacket) {
						packet.SetInputAsset(
							0, &fixture.activeProof.Asset,
						)
					},
				)
			},
			wantErr: "input asset does not match its proof tip",
		},
		{
			name: "output identity changed",
			mutate: func(t *testing.T,
				fixture *customAnchorPassiveFixture) {

				mutateCustomAnchorPassiveVPacket(
					t, fixture.request, func(packet *tappsbt.VPacket) {
						packet.Outputs[0].Asset.Genesis.Tag =
							"mutated-passive"
					},
				)
			},
			wantErr: "changed immutable asset identity",
		},
		{
			name: "output script key changed",
			mutate: func(t *testing.T,
				fixture *customAnchorPassiveFixture) {

				mutateCustomAnchorPassiveVPacket(
					t, fixture.request, func(packet *tappsbt.VPacket) {
						newKey := asset.NewScriptKeyBip86(
							keychain.KeyDescriptor{
								PubKey: testPrivateKey(
									t, 91,
								).PubKey(),
							},
						)
						packet.Outputs[0].ScriptKey = newKey
						packet.Outputs[0].Asset.ScriptKey = newKey
					},
				)
			},
			wantErr: "changed the asset script key",
		},
		{
			name: "output anchor changed",
			mutate: func(t *testing.T,
				fixture *customAnchorPassiveFixture) {

				mutateCustomAnchorPassiveVPacket(
					t, fixture.request, func(packet *tappsbt.VPacket) {
						packet.Outputs[0].AnchorOutputIndex = 0
					},
				)
			},
			wantErr: "does not reuse an active anchor output",
		},
		{
			name: "invalid witness",
			mutate: func(t *testing.T,
				fixture *customAnchorPassiveFixture) {

				mutateCustomAnchorPassiveVPacket(
					t, fixture.request, func(packet *tappsbt.VPacket) {
						witness := packet.Outputs[0].Asset.
							PrevWitnesses[0].TxWitness
						require.NotEmpty(t, witness)
						require.NotEmpty(t, witness[0])
						witness[0][0] ^= 1
					},
				)
			},
			wantErr: testValidateVirtualPacketWitnesses,
		},
		{
			name: "proof courier changed",
			mutate: func(_ *testing.T,
				fixture *customAnchorPassiveFixture) {

				fixture.request.PassiveAssets.Packets[0].ProofDelivery.
					CourierAddress =
					customAnchorPassiveOtherCourier
			},
			wantErr: "proof courier does not match requested delivery",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			fixture := newCustomAnchorPassiveFixture(t)
			test.mutate(t, fixture)

			_, err := NewWallet(fixture.client, NetworkRegtest).
				NewCustomAnchorTxBuilder().Build(
				context.Background(), fixture.request,
			)
			require.ErrorContains(t, err, test.wantErr)
		})
	}
}

func TestCustomAnchorPassiveCallerReanchorRequiresCoAnchor(t *testing.T) {
	t.Parallel()

	fixture := newCustomAnchorPassiveFixture(t)
	separateFile, separateProof, separateSpendKey :=
		newAssetProofPathBase(t)
	separatePacket := newCustomAnchorPassiveVPacket(
		t, fixture.request.Outputs[0], separateProof, separateSpendKey,
		customAnchorPassiveCourier,
	)
	separateRef, _, _, err := assetIdentity(&separateProof.Asset)
	require.NoError(t, err)
	fixture.request.PassiveAssets.Packets[0] = CustomAnchorPassivePacket{
		ID:          "separate-passive",
		AssetRef:    separateRef,
		Amount:      separateProof.Asset.Amount,
		VirtualPSBT: separatePacket,
		ProofFile:   separateFile,
		ProofDelivery: CustomAssetProofDelivery{
			CourierAddress: customAnchorPassiveCourier,
		},
	}

	_, err = NewWallet(fixture.client, NetworkRegtest).
		NewCustomAnchorTxBuilder().Build(
		context.Background(), fixture.request,
	)
	require.ErrorContains(t, err, "is not co-anchored with an active input")
}

func TestCustomAnchorPassivePackageBindsProofDelivery(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*testing.T, *CustomAnchorTransferPackage)
		wantErr string
	}{
		{
			name: "output summary courier",
			mutate: func(_ *testing.T,
				pkg *CustomAnchorTransferPackage) {

				pkg.Outputs[1].ProofDelivery.CourierAddress =
					customAnchorPassiveOtherCourier
			},
			wantErr: testOutputProofCourierDoesNotMatchOutputSummary,
		},
		{
			name: "virtual output courier",
			mutate: func(t *testing.T,
				pkg *CustomAnchorTransferPackage) {

				packet, err := tappsbt.Decode(
					pkg.PassiveVirtualPsbts[0],
				)
				require.NoError(t, err)
				packet.Outputs[0].ProofDeliveryAddress, err = url.Parse(
					customAnchorPassiveOtherCourier,
				)
				require.NoError(t, err)
				pkg.PassiveVirtualPsbts[0], err = tappsbt.Encode(packet)
				require.NoError(t, err)
			},
			wantErr: testOutputProofCourierDoesNotMatchOutputSummary,
		},
		{
			name: "proof update courier",
			mutate: func(_ *testing.T,
				pkg *CustomAnchorTransferPackage) {

				pkg.ProofUpdates[1].ProofDelivery.CourierAddress =
					customAnchorPassiveOtherCourier
			},
			wantErr: "proof update does not match output summary",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			pkg := newCommittedCustomAnchorPassivePackage(t)
			test.mutate(t, pkg)
			_, err := pkg.Seal()
			require.ErrorContains(t, err, test.wantErr)
		})
	}
}

func newCustomAnchorPassiveFixture(
	t *testing.T) *customAnchorPassiveFixture {

	t.Helper()

	activeFile, passiveFile, activeProof, passiveProof, activeSpendKey,
		passiveSpendKey := newCustomAnchorCoAnchoredProofs(t)
	require.Equal(t, activeProof.OutPoint(), passiveProof.OutPoint())
	require.Equal(t, activeProof.AnchorTx.TxHash(),
		passiveProof.AnchorTx.TxHash())

	activeRef, _, _, err := assetIdentity(&activeProof.Asset)
	require.NoError(t, err)
	passiveRef, _, _, err := assetIdentity(&passiveProof.Asset)
	require.NoError(t, err)
	require.False(t, activeRef.Equivalent(passiveRef))

	base := newCustomAnchorBuilderFixture(t)
	request := base.request.Clone()
	request.Inputs[0].AssetRef = activeRef
	request.Inputs[0].Amount = activeProof.Asset.Amount
	request.Inputs[0].ProofFile = cloneBytes(activeFile)
	request.Outputs[0].AssetRef = activeRef
	request.Outputs[0].Amount = activeProof.Asset.Amount
	request.AnchorPSBT = customAnchorCallerTemplate(
		t, []*proof.Proof{activeProof}, 3, 42,
		[]uint32{0x11111111, 0x22222222}, []wire.TxOut{
			{Value: 12_345, PkScript: []byte{txscript.OP_TRUE}},
			{Value: 777, PkScript: []byte{txscript.OP_FALSE}},
			{Value: 0, PkScript: cloneBytes(canonicalP2AScript)},
		},
	)
	request.SigningPlans[1].KeyPath.Signer = mustCustomAnchorXOnly(
		t, activeProof.InclusionProof.InternalKey,
	)
	passivePacket := newCustomAnchorPassiveVPacket(
		t, request.Outputs[0], passiveProof, passiveSpendKey,
		customAnchorPassiveCourier,
	)
	request.PassiveAssets = CustomAnchorPassiveAssets{
		Policy: CustomAnchorPassiveCallerReanchor,
		Packets: []CustomAnchorPassivePacket{{
			ID:          "passive-asset",
			AssetRef:    passiveRef,
			Amount:      passiveProof.Asset.Amount,
			VirtualPSBT: passivePacket,
			ProofFile:   cloneBytes(passiveFile),
			ProofDelivery: CustomAssetProofDelivery{
				RecipientID:    "passive-recipient",
				CourierAddress: customAnchorPassiveCourier,
				OpaqueMetadata: []byte{1, 2, 3},
			},
		}},
	}

	client := &customAnchorBuilderTestClient{
		verifyProof: customAnchorSuccessfulProofVerification,
		queryScriptKey: func(_ context.Context,
			key []byte) (*ScriptKey, error) {

			require.Equal(t, schnorr.SerializePubKey(
				activeProof.Asset.ScriptKey.PubKey,
			), key)
			resolved := customAnchorExternalScriptKey(t, activeSpendKey)
			return &resolved, nil
		},
	}

	return &customAnchorPassiveFixture{
		request:          request,
		client:           client,
		activeProofFile:  activeFile,
		passiveProofFile: passiveFile,
		activeProof:      activeProof,
		passiveProof:     passiveProof,
		activeSpendKey:   activeSpendKey,
		passiveSpendKey:  passiveSpendKey,
	}
}

func newCustomAnchorCoAnchoredProofs(t *testing.T) ([]byte, []byte,
	*proof.Proof, *proof.Proof, *btcec.PrivateKey, *btcec.PrivateKey) {

	t.Helper()

	var genesisHash chainhash.Hash
	genesisHash[0] = 0xc7
	genesisPoint := wire.OutPoint{
		Hash:  genesisHash,
		Index: 11,
	}
	activeGenesis := asset.Genesis{
		FirstPrevOut: genesisPoint,
		Tag:          "co-anchored-active",
		OutputIndex:  0,
		Type:         asset.Normal,
	}
	passiveGenesis := asset.Genesis{
		FirstPrevOut: genesisPoint,
		Tag:          "co-anchored-passive",
		OutputIndex:  0,
		Type:         asset.Normal,
	}
	activeAmount := uint64(100)
	passiveAmount := uint64(37)
	activeSpendKey := testPrivateKey(t, 71)
	passiveSpendKey := testPrivateKey(t, 72)
	version := commitment.TapCommitmentV2
	_, activeAssets, err := commitment.Mint(
		&version, activeGenesis, nil, &commitment.AssetDetails{
			Version: asset.V1,
			Type:    asset.Normal,
			ScriptKey: keychain.KeyDescriptor{
				PubKey: activeSpendKey.PubKey(),
			},
			Amount: &activeAmount,
		},
	)
	require.NoError(t, err)
	require.Len(t, activeAssets, 1)
	_, passiveAssets, err := commitment.Mint(
		&version, passiveGenesis, nil, &commitment.AssetDetails{
			Version: asset.V1,
			Type:    asset.Normal,
			ScriptKey: keychain.KeyDescriptor{
				PubKey: passiveSpendKey.PubKey(),
			},
			Amount: &passiveAmount,
		},
	)
	require.NoError(t, err)
	require.Len(t, passiveAssets, 1)

	tapCommitment, err := commitment.FromAssets(
		&version, activeAssets[0], passiveAssets[0],
	)
	require.NoError(t, err)
	anchorInternalKey := testPrivateKey(t, 2)
	anchorTx := assetProofPathAnchorTx(
		t, genesisPoint, anchorInternalKey.PubKey(), tapCommitment,
	)
	block := assetProofPathBlock(anchorTx)
	proofs, err := proof.NewMintingBlobs(
		&proof.MintParams{
			Block:            block,
			BlockHeight:      100,
			Tx:               anchorTx,
			TxIndex:          0,
			OutputIndex:      0,
			InternalKey:      anchorInternalKey.PubKey(),
			TaprootAssetRoot: tapCommitment,
			GenesisPoint:     genesisPoint,
		}, proof.MockVerifierCtx,
		proof.WithGenOption(proof.WithVersion(proof.TransitionV1)),
	)
	require.NoError(t, err)

	activeKey := asset.ToSerialized(activeAssets[0].ScriptKey.PubKey)
	activeProof, ok := proofs[activeKey]
	require.True(t, ok)
	passiveKey := asset.ToSerialized(passiveAssets[0].ScriptKey.PubKey)
	passiveProof, ok := proofs[passiveKey]
	require.True(t, ok)
	activeFile, err := proof.EncodeAsProofFile(activeProof)
	require.NoError(t, err)
	passiveFile, err := proof.EncodeAsProofFile(passiveProof)
	require.NoError(t, err)

	return activeFile, passiveFile, activeProof, passiveProof,
		activeSpendKey, passiveSpendKey
}

func newCustomAnchorPassiveVPacket(t *testing.T,
	output CustomAssetOutput, inputProof *proof.Proof,
	spendKey *btcec.PrivateKey, courierAddress string) []byte {

	t.Helper()

	internalKey := mustParseCustomAnchorPubKey(
		t, output.Anchor.InternalKey.PubKey,
	)
	var proofURL *url.URL
	if courierAddress != "" {
		var err error
		proofURL, err = url.Parse(courierAddress)
		require.NoError(t, err)
	}
	packets, err := tapsend.DistributeCoins(
		[]*proof.Proof{inputProof}, []*tapsend.Allocation{{
			Type:        tapsend.CommitAllocationToRemote,
			OutputIndex: output.AnchorOutputIndex,
			InternalKey: internalKey,
			GenScriptKey: tapsend.StaticScriptKeyGen(
				inputProof.Asset.ScriptKey,
			),
			Amount:               inputProof.Asset.Amount,
			AssetVersion:         asset.V1,
			BtcAmount:            btcutil.Amount(output.AnchorValueSat),
			ProofDeliveryAddress: proofURL,
		}}, &address.RegressionNetTap, true, tappsbt.V1,
	)
	require.NoError(t, err)
	require.Len(t, packets, 1)
	require.NoError(t, tapsend.PrepareOutputAssets(
		context.Background(), packets[0],
	))
	encoded, err := tappsbt.Encode(packets[0])
	require.NoError(t, err)

	return customAnchorSignVirtualPacket(t, spendKey, encoded)
}

func mutateCustomAnchorPassiveVPacket(t *testing.T,
	request *CustomAnchorRequest, mutate func(*tappsbt.VPacket)) {

	t.Helper()

	packet, err := tappsbt.Decode(
		request.PassiveAssets.Packets[0].VirtualPSBT,
	)
	require.NoError(t, err)
	mutate(packet)
	request.PassiveAssets.Packets[0].VirtualPSBT, err =
		tappsbt.Encode(packet)
	require.NoError(t, err)
}

func newCommittedCustomAnchorPassivePackage(
	t *testing.T) *CustomAnchorTransferPackage {

	t.Helper()

	fixture := newCustomAnchorPassiveFixture(t)
	fixture.client.signVirtual = func(_ context.Context, packet []byte) (
		[]byte, error) {

		return customAnchorSignVirtualPacket(
			t, fixture.activeSpendKey, packet,
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
	pkg, err := plan.Commit(context.Background())
	require.NoError(t, err)
	require.NoError(t, pkg.Validate())

	return pkg
}
