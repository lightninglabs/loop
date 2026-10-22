package tapsdk

import (
	"context"
	"testing"

	sdk "github.com/lightninglabs/tap-sdk"
	"github.com/stretchr/testify/require"
)

func TestDirectionNeutralProofBindings(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*InputBinding)
	}{
		{name: testValid},
		{"issuance", func(b *InputBinding) { b.IssuanceID[0] ^= 1 }},
		{"amount", func(b *InputBinding) { b.Amount++ }},
		{"unexpected group", func(b *InputBinding) { k := testKey(t, 11); b.GroupKey = &k }},
		{"outpoint", func(b *InputBinding) { b.AnchorOutpoint.Index++ }},
		{"script key", func(b *InputBinding) { b.ScriptKey = testKey(t, 12) }},
		{"contract authority", func(b *InputBinding) { b.Anchor.InternalKey = testKey(t, 12) }},
		{"spend root", func(b *InputBinding) { b.Spend.TaprootMerkleRoot[0] ^= 1 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newPathFixture(t)
			binding := f.binding.clone()
			if test.change != nil {
				test.change(&binding)
			}
			_, err := VerifyInputProof(
				t.Context(),
				f.wallet,
				f.chain,
				2,
				f.request.Inputs[0].ProofFile,
				binding,
				true,
			)
			if test.change == nil {
				require.NoError(t, err)
				require.Equal(t, 1, f.client.verifies)
			} else {
				require.Error(t, err)
				require.Zero(t, f.client.verifies)
			}
		})
	}
}

type unsupportedNode struct{ sdk.Client }

func (*unsupportedNode) GetInfo(context.Context) (*sdk.Info, error) {
	return &sdk.Info{Version: "0.8.5", Network: "regtest"}, nil
}

func TestLiveFloorPreservesLocalRecovery(t *testing.T) {
	wallet := sdk.NewWallet(&unsupportedNode{}, sdk.NetworkRegtest)
	require.ErrorContains(t, CheckNodeVersion(t.Context(), wallet), "0.8.6")
	// Native saved-package and witness decoding has no tapd-version gate.
	_, final := nativeSignedFixture(t)
	raw, err := final.MarshalBinary()
	require.NoError(t, err)
	recovered, err := DecodeFinalPackage(raw)
	require.NoError(t, err)
	require.Equal(
		t,
		final.CommittedPackageDigest,
		recovered.CommittedPackageDigest,
	)
}

func TestApprovalChecksBitcoinOnlyFunding(t *testing.T) {
	f := newPathFixture(t)
	require.NoError(
		t,
		CheckInputsCurrent(t.Context(), f.chain, 2, f.request, true),
	)
	delete(f.chain.utxos, requestPoint(t, f.request, 0))
	require.Error(
		t,
		CheckInputsCurrent(t.Context(), f.chain, 2, f.request, true),
	)
	// Exact plan approval also caps a real nonzero native transfer fee.
	f = newPathFixture(t)
	template, err := DecodePSBT(f.request.AnchorPSBT)
	require.NoError(t, err)
	key, spend, err := LegacyOPTrueDescriptor()
	require.NoError(t, err)
	output := f.request.Outputs[0]
	approved := OutputBinding{
		ID:                output.ID,
		IssuanceID:        f.binding.IssuanceID,
		AssetRef:          output.AssetRef,
		Amount:            output.Amount,
		AnchorOutputIndex: output.AnchorOutputIndex,
		AnchorValueSat:    int64(output.AnchorValueSat),
		ScriptMode:        sdk.CustomAssetScriptExternal,
		ScriptKey:         key,
		Anchor:            f.kit.AnchorPlan(),
		OPTrue:            spend,
	}
	plan, err := f.wallet.NewCustomAnchorTxBuilder().
		Build(t.Context(), f.request)
	require.NoError(t, err)
	require.NoError(
		t,
		ApprovePlan(
			plan,
			template.UnsignedTx,
			[]OutputBinding{approved},
			2_000,
		),
	)
	require.Error(
		t,
		ApprovePlan(
			plan,
			template.UnsignedTx,
			[]OutputBinding{approved},
			1_999,
		),
	)
}
