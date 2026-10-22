package tapsdk

import (
	"bytes"
	"crypto/sha256"
	"testing"

	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/lightninglabs/loop/swapkit"
	sdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/taproot-assets/tappsbt"
	"github.com/lightninglabs/taproot-assets/tapsend"
	"github.com/stretchr/testify/require"
)

// TestSwapKitSigningPlan checks SDK signing metadata and defensive copies.
// Bitcoin policy vectors and VM execution are covered by the swapkit module.
func TestSwapKitSigningPlan(t *testing.T) {
	kit, err := swapkit.NewSwapKit(swapKitTestParams(t))
	require.NoError(t, err)
	source := kit.AnchorPlan()
	plan := AnchorPlan(source)
	expected := bytes.Clone(plan.Tapscript.TapLeaves[0].Script)
	plan.Tapscript.TapLeaves[0].Script[0] ^= 1
	require.Equal(t, expected,
		source.Leaves[0])

	context := []byte("unique-signing-round")
	signing := CooperativeSigningPlan(
		kit.CooperativeParticipants(), 2, context,
	)
	require.Equal(t, uint32(2), signing.InputIndex)
	require.NoError(t, validateMuSig2Aggregate(
		sdk.PubKey(source.InternalKey).XOnly(),
		signing.MuSig2.Participants,
	))
	context[0] ^= 1
	require.NotEqual(t, context, signing.MuSig2.SessionContext)
	signing.MuSig2.Participants[0] = sdk.PubKey{}
	require.NotEqual(t, signing.MuSig2.Participants,
		CooperativeSigningPlan(
			kit.CooperativeParticipants(), 2, nil,
		).MuSig2.Participants)
}

// TestSwapKitBuilderCommitments binds the kit's Bitcoin control blocks to the
// custom-anchor builder's independently assembled asset and policy roots.
func TestSwapKitBuilderCommitments(t *testing.T) {
	params := swapKitTestParams(t)
	legacy, err := swapkit.NewSwapKit(params)
	require.NoError(t, err)
	params.Policy = swapkit.SwapHTLCStaticLoopInV3
	v3, err := swapkit.NewSwapKit(params)
	require.NoError(t, err)
	deposit, err := swapkit.NewDepositKit(swapkit.DepositKitParams{
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
	AnchorPlan() swapkit.AnchorPlan
	TimeoutSpend(swapkit.Hash) (*swapkit.SwapAnchorSpendInfo, error)
}

func testSwapKitBuilderCommitment(t *testing.T, kit swapKitOutput,
	scalar byte) sdk.PubKey {

	t.Helper()
	fixture := newCustomAnchorBuilderFixture(t)
	original := fixture.request.Outputs[0]
	output := OPTrueOutput(original, sdk.KeyDescriptor{
		RawKeyBytes: testKey(t, scalar),
	}, kit.AnchorPlan())
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

	// Derive the builder's root independently through tapd's public
	// commitment implementation. No signing or backend commit is needed.
	packets := plan.ActiveVirtualPSBTs()
	require.Len(t, packets, 1)
	packet, err := tappsbt.NewFromRawBytes(
		bytes.NewReader(packets[0]),
		false,
	)
	require.NoError(t, err)
	commitments, err := tapsend.CreateOutputCommitments(
		[]*tappsbt.VPacket{packet}, tapsend.WithSpenderLeaves(),
	)
	require.NoError(t, err)
	virtual := packet.Outputs[planned.VirtualOutputIndex]
	_, merkle, root, err := tapsend.AnchorOutputScript(
		virtual.AnchorOutputInternalKey,
		virtual.AnchorOutputTapscriptSibling,
		commitments[virtual.AnchorOutputIndex],
	)
	require.NoError(t, err)
	spend, err := kit.TimeoutSpend(sdk.Hash(root))
	require.NoError(t, err)
	require.Equal(t, [32]byte(merkle), spend.TaprootMerkleRoot)
	signing := ScriptSigningPlan(spend, 3)
	require.Equal(t, uint32(3), signing.InputIndex)
	require.Equal(t, sdk.Hash(txscript.NewBaseTapLeaf(
		spend.LeafScript,
	).TapHash()), signing.ScriptPath.LeafHash)
	require.Equal(t, []sdk.XOnlyPubKey{
		sdk.XOnlyPubKey(spend.RequiredSigner),
	}, signing.ScriptPath.RequiredSigners)
	control, err := txscript.ParseControlBlock(spend.ControlBlock)
	require.NoError(t, err)
	require.NoError(t, txscript.VerifyTaprootLeafCommitment(
		control, spend.PkScript[2:], spend.LeafScript,
	))
	return planned.ScriptKey
}

func swapKitTestParams(t *testing.T) swapkit.SwapKitParams {
	t.Helper()
	sender := testKey(t, 1)
	receiver := testKey(t, 2)
	preimage := [32]byte{42}
	return swapkit.SwapKitParams{
		Policy:    swapkit.SwapHTLCLegacyCSV,
		SenderKey: sender, ReceiverKey: receiver,
		SenderInternalKey: sender, ReceiverInternalKey: receiver,
		PaymentHash: sdk.Hash(sha256.Sum256(preimage[:])),
		Expiry:      144,
	}
}
