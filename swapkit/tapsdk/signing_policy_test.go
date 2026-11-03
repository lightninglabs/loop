package tapsdk

import (
	"testing"

	"github.com/btcsuite/btcd/txscript"
	sdk "github.com/lightninglabs/tap-sdk"
	"github.com/stretchr/testify/require"
)

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
