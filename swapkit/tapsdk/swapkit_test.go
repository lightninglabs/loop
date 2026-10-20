package tapsdk

import (
	"bytes"
	"crypto/sha256"
	"math"
	"testing"

	"github.com/btcsuite/btcd/txscript/v2"
	sdk "github.com/lightninglabs/tap-sdk"
	"github.com/stretchr/testify/require"
)

// TestSwapKitSigningPlan checks SDK signing metadata and defensive copies.
// Bitcoin policy vectors and VM execution are covered by the swapkit module.
func TestSwapKitSigningPlan(t *testing.T) {
	kit, err := NewSwapKit(swapKitTestParams(t))
	require.NoError(t, err)
	plan := kit.AnchorPlan()
	expected := bytes.Clone(plan.Tapscript.TapLeaves[0].Script)
	plan.Tapscript.TapLeaves[0].Script[0] ^= 1
	require.Equal(t, expected,
		kit.AnchorPlan().Tapscript.TapLeaves[0].Script)

	context := []byte("unique-signing-round")
	signing := kit.CooperativeSigningPlan(2, context)
	require.Equal(t, uint32(2), signing.InputIndex)
	require.NoError(t, validateMuSig2Aggregate(
		kit.AnchorPlan().InternalKey.PubKey.XOnly(),
		signing.MuSig2.Participants,
	))
	context[0] ^= 1
	require.NotEqual(t, context, signing.MuSig2.SessionContext)
	signing.MuSig2.Participants[0] = sdk.PubKey{}
	require.NotEqual(t, signing.MuSig2.Participants,
		kit.CooperativeSigningPlan(2, nil).MuSig2.Participants)
}

// TestSwapKitOutput checks OP_TRUE output descriptors, native amount bounds,
// and defensive copies before passing them to the custom-anchor builder.
func TestSwapKitOutput(t *testing.T) {
	kit, err := NewSwapKit(swapKitTestParams(t))
	require.NoError(t, err)
	params := SwapOutputParams{
		ID:       "deposit",
		AssetRef: sdk.AssetRefFromAssetID(sdk.AssetID{1}),
		Amount:   1_000, AnchorValueSat: 330,
		AssetInternalKey: sdk.KeyDescriptor{RawKeyBytes: testKey(t, 3)},
		ProofDelivery: sdk.CustomAssetProofDelivery{
			OpaqueMetadata: []byte{1},
		},
	}
	output, err := kit.Output(params)
	require.NoError(t, err)
	require.Equal(t, sdk.CustomAssetScriptOPTrue, output.Script.Mode)
	require.Equal(t, sdk.CustomAssetTimelocks{}, output.Timelocks)
	require.Equal(t, kit.AnchorPlan(), output.Anchor)
	params.ProofDelivery.OpaqueMetadata[0] = 2
	require.Equal(t, []byte{1}, output.ProofDelivery.OpaqueMetadata)
	params.Amount = math.MaxInt64 + 1
	_, err = kit.Output(params)
	require.ErrorContains(t, err, "amount exceeds maximum")
	params.Amount = 1
	params.AnchorValueSat = math.MaxInt64 + 1
	_, err = kit.Output(params)
	require.ErrorContains(t, err, "amount exceeds maximum")
	params.AnchorValueSat = 330
	params.AssetInternalKey = sdk.KeyDescriptor{}
	_, err = kit.Output(params)
	require.ErrorContains(t, err, "OP_TRUE internal key")
}

// TestSwapKitBuilderCommitments binds the kit's Bitcoin control blocks to the
// custom-anchor builder's independently assembled asset and policy roots.
func TestSwapKitBuilderCommitments(t *testing.T) {
	params := swapKitTestParams(t)
	legacy, err := NewSwapKit(params)
	require.NoError(t, err)
	params.Policy = SwapHTLCStaticLoopInV3
	v3, err := NewSwapKit(params)
	require.NoError(t, err)
	deposit, err := NewDepositKit(DepositKitParams{
		ClientKey: params.SenderKey, ServerKey: params.ReceiverKey,
		CSVExpiry: 144,
	})
	require.NoError(t, err)
	for name, kit := range map[string]swapKitOutput{
		"legacy": legacy, "v3": v3, "deposit": deposit,
	} {
		t.Run(name, func(t *testing.T) {
			first := testSwapKitBuilderCommitment(t, kit, 10)
			second := testSwapKitBuilderCommitment(t, kit, 11)
			require.NotEqual(t, first, second)
		})
	}
}

type swapKitOutput interface {
	Output(SwapOutputParams) (sdk.CustomAssetOutput, error)
	TimeoutSpend(sdk.Hash) (*SwapAnchorSpendInfo, error)
}

func testSwapKitBuilderCommitment(t *testing.T, kit swapKitOutput,
	scalar byte) sdk.PubKey {

	t.Helper()
	fixture := newCustomAnchorBuilderFixture(t)
	original := fixture.request.Outputs[0]
	output, err := kit.Output(SwapOutputParams{
		ID: original.ID, AssetRef: original.AssetRef,
		Amount:            original.Amount,
		AnchorOutputIndex: original.AnchorOutputIndex,
		AnchorValueSat:    original.AnchorValueSat,
		AssetInternalKey: sdk.KeyDescriptor{
			RawKeyBytes: testKey(t, scalar),
		},
	})
	require.NoError(t, err)
	fixture.request.Outputs[0] = output
	wallet := sdk.NewWallet(fixture.client, sdk.NetworkRegtest)
	plan, err := wallet.NewCustomAnchorTxBuilder().
		Build(t.Context(), fixture.request)
	require.NoError(t, err)
	planned := plan.Outputs()[0]
	require.NotNil(t, planned.OPTrueSpend)
	require.NoError(t, planned.OPTrueSpend.Validate(planned.ScriptKey))
	require.Equal(t, []byte{txscript.OP_TRUE},
		planned.OPTrueSpend.LeafScript)

	preview, err := plan.PreviewOutputCommitments()
	require.NoError(t, err)
	require.Len(t, preview, 1)
	spend, err := kit.TimeoutSpend(preview[0].TaprootAssetRoot)
	require.NoError(t, err)
	require.Equal(t, preview[0].TaprootMerkleRoot, spend.TaprootMerkleRoot)
	control, err := txscript.ParseControlBlock(spend.ControlBlock)
	require.NoError(t, err)
	require.NoError(t, txscript.VerifyTaprootLeafCommitment(
		control, spend.PkScript[2:], spend.LeafScript,
	))
	return planned.ScriptKey
}

func swapKitTestParams(t *testing.T) SwapKitParams {
	t.Helper()
	sender := testKey(t, 1)
	receiver := testKey(t, 2)
	preimage := [32]byte{42}
	return SwapKitParams{
		Policy:    SwapHTLCLegacyCSV,
		SenderKey: sender, ReceiverKey: receiver,
		SenderInternalKey: sender, ReceiverInternalKey: receiver,
		PaymentHash: sdk.Hash(sha256.Sum256(preimage[:])),
		Expiry:      144,
	}
}
