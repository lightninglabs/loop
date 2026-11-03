package tapsdk

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcutil/psbt"
	sdk "github.com/lightninglabs/tap-sdk"
	"github.com/stretchr/testify/require"
)

// nativeSignedFixture completes the public package using the existing LND
// session fixture. Both packages retain the same committed transaction.
func nativeSignedFixture(t *testing.T) (*sdk.CustomAnchorTransferPackage,
	*sdk.CustomAnchorTransferPackage) {

	t.Helper()
	approved := signingPackage(t)
	signed, err := signTestPackage(
		t.Context(),
		signingWallet(t, 2),
		signingWallet(t, 3),
		approved,
	)
	require.NoError(t, err)
	finalized, err := approved.WithFinalAnchorPSBT(
		finishSigningFixture(t, signed),
	)
	require.NoError(t, err)
	return approved, finalized
}

// TestNativeFinalPackageBindsApprovedPolicy checks the claim-acceptance helper
// used by VerifyClaim. A valid final witness and matching unsigned transaction
// do not authorize replacing the approved signing policy or package metadata.
func TestNativeFinalPackageBindsApprovedPolicy(t *testing.T) {
	approved, finalized := nativeSignedFixture(t)
	raw, err := finalized.MarshalBinary()
	require.NoError(t, err)
	bound, err := BindFinalPackage(approved, raw)
	require.NoError(t, err)
	require.Equal(t, finalized.PackageDigest, bound.PackageDigest)
	require.Equal(t, approved.CommittedPackageDigest,
		bound.CommittedPackageDigest)
	require.NotEqual(t, approved.PackageDigest, bound.PackageDigest)

	for _, test := range []struct {
		name   string
		change func(*sdk.CustomAnchorTransferPackage)
	}{
		{
			name: "publish metadata",
			change: func(p *sdk.CustomAnchorTransferPackage) {
				p.Publish.Label += " changed"
			},
		},
		{
			name: "signing session context",
			change: func(p *sdk.CustomAnchorTransferPackage) {
				for i := range p.SigningPlans {
					plan := &p.SigningPlans[i]
					if plan.MuSig2 != nil {
						plan.MuSig2.SessionContext = []byte(
							"another approved operation",
						)
					}
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := finalized.Clone()
			test.change(changed)
			changed, err := changed.Seal()
			require.NoError(t, err)
			require.Equal(t, approved.UnsignedTxDigest,
				changed.UnsignedTxDigest)
			require.NotEqual(t, approved.CommittedPackageDigest,
				changed.CommittedPackageDigest)
			// The substitute is internally valid, including its final
			// witnesses. Only binding to the approved policy rejects it.
			_, err = changed.WithFinalAnchorPSBT(changed.AnchorPsbt)
			require.NoError(t, err)
			raw, err := changed.MarshalBinary()
			require.NoError(t, err)
			bound, err := BindFinalPackage(approved, raw)
			require.Error(t, err)
			require.Nil(t, bound)
		})
	}
}

// TestNativeClaimPackageRequiresFinalWitness prevents validated SDK package
// decoding from being mistaken for full witness validation at claim acceptance.
func TestNativeClaimPackageRequiresFinalWitness(t *testing.T) {
	approved, finalized := nativeSignedFixture(t)
	for _, name := range []string{"unsigned", "partial", "invalid witness"} {
		t.Run(name, func(t *testing.T) {
			proposed := approved.Clone()
			switch name {
			case "unsigned":
				proposed.AnchorPsbt = proposed.CommittedAnchorPsbt
			case "invalid witness":
				proposed = finalized.Clone()
				packet, err := DecodePSBT(proposed.AnchorPsbt)
				require.NoError(t, err)
				var witness bytes.Buffer
				require.NoError(t, psbt.WriteTxWitness(
					&witness, [][]byte{{1}},
				))
				packet.Inputs[0].FinalScriptWitness = witness.Bytes()
				proposed.AnchorPsbt, err = EncodePSBT(packet)
				require.NoError(t, err)
			}
			proposed, err := proposed.Seal()
			require.NoError(t, err)
			raw, err := proposed.MarshalBinary()
			require.NoError(t, err)
			var decoded sdk.CustomAnchorTransferPackage
			require.NoError(t, decoded.UnmarshalBinary(raw))
			require.Equal(t, approved.CommittedPackageDigest,
				decoded.CommittedPackageDigest)
			bound, err := BindFinalPackage(approved, raw)
			require.Error(t, err)
			require.Nil(t, bound)
		})
	}
}
