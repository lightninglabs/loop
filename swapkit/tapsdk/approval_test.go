package tapsdk

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"

	"github.com/lightninglabs/loop/swapkit"
	sdk "github.com/lightninglabs/tap-sdk"
	"github.com/stretchr/testify/require"
)

func newPlanApprovalFixture(t *testing.T) (*builderFixture,
	*psbt.Packet, OutputBinding) {

	t.Helper()
	fixture := newCustomAnchorBuilderFixture(t)
	kit, err := swapkit.NewSwapKit(swapKitTestParams(t))
	require.NoError(t, err)
	fixture.request.Outputs[0], err = LegacyOPTrueOutput(
		fixture.request.Outputs[0],
		kit.AnchorPlan(),
	)
	require.NoError(t, err)
	template, err := DecodePSBT(fixture.request.AnchorPSBT)
	require.NoError(t, err)
	input, err := LastProof(fixture.request.Inputs[0].ProofFile)
	require.NoError(t, err)
	key, spend, err := LegacyOPTrueDescriptor()
	require.NoError(t, err)
	output := fixture.request.Outputs[0]
	approval := OutputBinding{
		ID:                output.ID,
		IssuanceID:        sdk.AssetID(input.Asset.ID()),
		AssetRef:          output.AssetRef,
		Amount:            output.Amount,
		AnchorOutputIndex: output.AnchorOutputIndex,
		AnchorValueSat:    int64(output.AnchorValueSat),
		ScriptMode:        sdk.CustomAssetScriptExternal,
		ScriptKey:         key,
		Anchor:            kit.AnchorPlan(),
		OPTrue:            spend,
	}
	return fixture, template, approval
}

func TestPlanRequiresIndependentApproval(t *testing.T) {
	fixture, template, approval := newPlanApprovalFixture(t)
	wallet := sdk.NewWallet(fixture.client, sdk.NetworkRegtest)
	plan, err := wallet.NewCustomAnchorTxBuilder().
		Build(t.Context(), fixture.request)
	require.NoError(t, err)
	require.NoError(
		t,
		ApprovePlan(
			plan,
			template.UnsignedTx,
			[]OutputBinding{approval},
			100_000,
		),
	)
	for _, test := range []struct {
		name   string
		change func(*OutputBinding)
	}{
		{"issuance", func(a *OutputBinding) { a.IssuanceID[0] ^= 1 }},
		{"amount", func(a *OutputBinding) { a.Amount++ }},
		{"script key", func(a *OutputBinding) { a.ScriptKey.PubKey = testKey(t, 12) }},
		{"raw key parity", func(a *OutputBinding) { a.ScriptKey.KeyDesc.RawKeyBytes[0] ^= 1 }},
		{"wallet locator", func(a *OutputBinding) { a.ScriptKey.KeyDesc.KeyLocator.Index++ }},
		{"Bitcoin contract", func(a *OutputBinding) { a.Anchor.InternalKey = testKey(t, 11) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			altered := approval
			test.change(&altered)
			require.Error(
				t,
				ApprovePlan(
					plan,
					template.UnsignedTx,
					[]OutputBinding{altered},
					100_000,
				),
			)
		})
	}
	changed := template.UnsignedTx.Copy()
	changed.TxIn[0].Sequence--
	require.Error(
		t,
		ApprovePlan(plan, changed, []OutputBinding{approval}, 100_000),
	)
	changed = template.UnsignedTx.Copy()
	changed.TxOut[0].Value--
	require.Error(
		t,
		ApprovePlan(plan, changed, []OutputBinding{approval}, 100_000),
	)
	require.NoError(
		t,
		ApprovePlan(
			plan,
			template.UnsignedTx,
			[]OutputBinding{approval},
			0,
		),
	)
	// A deferred wallet approval must actually select wallet funding.
	require.Error(
		t,
		ApproveWalletFundedPlan(
			plan,
			template.UnsignedTx,
			[]OutputBinding{approval},
		),
	)
	require.Equal(t, 1, fixture.client.verifyCalls)
}

func TestPlanRequiresDefaultSigningPolicy(t *testing.T) {
	for _, hash := range []txscript.SigHashType{
		txscript.SigHashAll, txscript.SigHashNone,
		txscript.SigHashSingle,
		txscript.SigHashAll | txscript.SigHashAnyOneCanPay,
	} {
		t.Run(fmt.Sprintf("hash%d", hash), func(t *testing.T) {
			fixture, template, approval := newPlanApprovalFixture(t)
			template.Inputs[0].SighashType = hash
			var err error
			fixture.request.AnchorPSBT, err = EncodePSBT(template)
			require.NoError(t, err)
			wallet := sdk.NewWallet(fixture.client, sdk.NetworkRegtest)
			plan, err := wallet.NewCustomAnchorTxBuilder().Build(
				t.Context(), fixture.request,
			)
			require.NoError(t, err)
			require.ErrorIs(t, ApprovePlan(plan, template.UnsignedTx,
				[]OutputBinding{approval}, 100_000), ErrSigningRequest)
			require.ErrorIs(t, ApproveWalletFundedPlan(plan,
				template.UnsignedTx, []OutputBinding{approval}),
				ErrSigningRequest)
		})
	}
}

func TestWalletFundingChangeApproval(t *testing.T) {
	for name, mode := range map[string]sdk.AnchorChangeOutputMode{
		"new wallet change": sdk.AnchorChangeOutputAdd,
		"existing output":   sdk.AnchorChangeOutputExisting,
		"no new output":     sdk.AnchorChangeOutputNoNew,
	} {
		t.Run(name, func(t *testing.T) {
			fixture, template, approval := newPlanApprovalFixture(t)
			fee, err := sdk.NewFeeRateSatPerVByte(1)
			require.NoError(t, err)
			fixture.request.Funding = sdk.CustomAnchorFundingPlan{
				Mode: sdk.CustomAnchorFundingWalletFunded,
				WalletFunded: &sdk.CustomAnchorWalletFunding{
					ChangeOutput: sdk.AnchorChangeOutput{
						Mode: mode, ExistingOutputIndex: 2,
					},
					Fee: sdk.AnchorFee{
						Mode: sdk.AnchorFeeSatPerVByte, FeeRate: fee,
					},
					MaxFeeSat:    10_000,
					CustomLockID: bytes.Repeat([]byte{1}, 32),
				},
			}
			wallet := sdk.NewWallet(fixture.client, sdk.NetworkRegtest)
			plan, err := wallet.NewCustomAnchorTxBuilder().Build(
				t.Context(), fixture.request,
			)
			require.NoError(t, err)
			err = ApproveWalletFundedPlan(plan, template.UnsignedTx,
				[]OutputBinding{approval})
			if mode == sdk.AnchorChangeOutputAdd {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "newly added wallet change")
			}
		})
	}
}

func TestFundedPackagePolicy(t *testing.T) {
	f := newPathFixture(t)
	plan, err := f.wallet.NewCustomAnchorTxBuilder().Build(
		t.Context(), f.request,
	)
	require.NoError(t, err)
	pkg, err := plan.Commit(t.Context())
	require.NoError(t, err)
	require.Error(t, CheckFundedPackage(pkg, 2_000))
	pkg.Funding.Mode = sdk.CustomAnchorFundingWalletFunded
	pkg.Funding.MaxFeeSat = 2_000
	pkg.FundingLock.CustomLockID = bytes.Repeat([]byte{1}, 32)
	pkg, err = pkg.Seal()
	require.NoError(t, err)
	require.NoError(t, CheckFundedPackage(pkg, 2_000))
	require.Error(t, CheckFundedPackage(pkg, 1_999))
	changedHash := withSigningHash(t, pkg, pkg.SigningPlans[0].InputIndex,
		txscript.SigHashAll)
	require.ErrorIs(t, CheckFundedPackage(changedHash, 2_000),
		ErrSigningRequest)
	for _, index := range []int32{0, 1} {
		changed := pkg.Clone()
		changed.ChangeOutputIndex = index
		changed, err = changed.Seal()
		require.NoError(t, err)
		require.Error(t, CheckFundedPackage(changed, 2_000))
	}
}

func TestBackendFeeSigningPolicy(t *testing.T) {
	f := newPathFixture(t)
	plan, err := f.wallet.NewCustomAnchorTxBuilder().Build(
		t.Context(), f.request,
	)
	require.NoError(t, err)
	pkg, err := plan.Commit(t.Context())
	require.NoError(t, err)
	packet, err := DecodePSBT(pkg.CommittedAnchorPsbt)
	require.NoError(t, err)
	fee := packet.UnsignedTx.TxIn[0].PreviousOutPoint
	pkg.Funding.Mode = sdk.CustomAnchorFundingWalletFunded
	pkg.Funding.MaxFeeSat = 2_000
	pkg.FundingLock.CustomLockID = bytes.Repeat([]byte{1}, 32)
	pkg.BackendManagedInputIndices = []uint32{0}
	pkg.LockedUTXOs = []sdk.CustomAnchorLockedUTXO{{
		Outpoint: sdk.Outpoint{Txid: sdk.Hash(fee.Hash), Index: fee.Index},
	}}
	pkg.SigningPlans = pkg.SigningPlans[1:]
	// The wallet owns its fee-input signatures; the explicit contract plan
	// still uses SIGHASH_DEFAULT.
	pkg = withSigningHash(t, pkg, 0, txscript.SigHashAll)
	require.NoError(t, CheckFundedPackage(pkg, 2_000))
}
