package tapsdk

import (
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/txscript/v2"
	sdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

func signingPolicyPackage(
	t *testing.T, file string,
) *sdk.CustomAnchorTransferPackage {

	t.Helper()
	raw, err := signingFixtures.ReadFile("testdata/" + file + ".package")
	require.NoError(t, err)
	var pkg sdk.CustomAnchorTransferPackage
	require.NoError(t, pkg.UnmarshalBinary(raw))
	return &pkg
}

func withSigningHash(t *testing.T, pkg *sdk.CustomAnchorTransferPackage,
	index uint32, hash txscript.SigHashType) *sdk.CustomAnchorTransferPackage {

	t.Helper()
	pkg = pkg.Clone()
	packet, err := DecodePSBT(pkg.CommittedAnchorPsbt)
	require.NoError(t, err)
	packet.Inputs[index].SighashType = hash
	raw, err := EncodePSBT(packet)
	require.NoError(t, err)
	pkg.CommittedAnchorPsbt, pkg.AnchorPsbt = raw, raw
	pkg, err = pkg.Seal()
	require.NoError(t, err)
	return pkg
}

func TestPackageSigningPolicy(t *testing.T) {
	require.ErrorIs(t, CheckSigningPolicy(nil), ErrSigningRequest)
	for _, file := range []string{"musig", "script"} {
		original := signingPolicyPackage(t, file)
		for _, plan := range original.SigningPlans {
			for _, hash := range []txscript.SigHashType{
				txscript.SigHashDefault, txscript.SigHashAll,
				txscript.SigHashNone, txscript.SigHashSingle,
				txscript.SigHashAll | txscript.SigHashAnyOneCanPay,
			} {
				name := fmt.Sprintf("%s/input%d/hash%d", file,
					plan.InputIndex, hash)
				t.Run(name, func(t *testing.T) {
					pkg := withSigningHash(t, original,
						plan.InputIndex, hash)
					err := CheckSigningPolicy(pkg)
					if hash == txscript.SigHashDefault {
						require.NoError(t, err)
					} else {
						require.ErrorIs(t, err, ErrSigningRequest)
					}
				})
			}
		}
	}
}

func TestScriptSigningChecksPolicyBeforeSigner(t *testing.T) {
	pkg := signingPolicyPackage(t, "script")
	var index uint32
	for _, plan := range pkg.SigningPlans {
		if plan.ScriptPath != nil {
			index = plan.InputIndex
			break
		}
	}
	pkg = withSigningHash(t, pkg, index, txscript.SigHashAll)
	wallet := &scriptWallet{}
	final, err := FinalizeScriptSpend(t.Context(), wallet,
		keychain.KeyLocator{}, pkg, nil)
	require.ErrorIs(t, err, ErrSigningRequest)
	require.Nil(t, final)
	require.Zero(t, wallet.calls)
}

func TestCallerProvidedSigningPolicy(t *testing.T) {
	pkg := signingPolicyPackage(t, "musig")
	plan := &pkg.SigningPlans[0]
	plan.KeyPath, plan.MuSig2, plan.ScriptPath = nil, nil, nil
	plan.CallerSigned = &sdk.CustomAnchorCallerSignedPlan{}
	pkg = withSigningHash(t, pkg, plan.InputIndex, txscript.SigHashAll)
	require.ErrorIs(t, CheckSigningPolicy(pkg), ErrSigningRequest)
}
