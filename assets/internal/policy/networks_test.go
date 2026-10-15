package policy

import (
	"testing"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightninglabs/taproot-assets/address"
	"github.com/stretchr/testify/require"
)

// TestCanonicalAddressParams verifies that supported network tuples resolve to
// independent copies of their canonical parameters, and that incomplete or
// mismatched tuples are rejected.
func TestCanonicalAddressParams(t *testing.T) {
	simNetCoinType := address.SimNetTap.HDCoinType

	rawSimNet := CloneAddressParams(address.SimNetTap)
	rawSimNet.HDCoinType = RawSimNetHDCoinType
	lndSimNet := CloneAddressParams(address.SimNetTap)
	lndSimNet.HDCoinType = address.TestNet3Tap.HDCoinType

	for _, network := range []*address.ChainParams{
		&address.MainNetTap,
		&address.TestNet3Tap,
		&address.TestNet4Tap,
		&address.RegressionNetTap,
		&address.SigNetTap,
		rawSimNet,
		lndSimNet,
	} {
		canonical, err := CanonicalAddressParams(network)
		require.NoError(t, err)
		require.Equal(t, network.TapHRP, canonical.TapHRP)
		require.Equal(t, network.Net, canonical.Net)
		require.Equal(t, network.HDCoinType, canonical.HDCoinType)
		require.Equal(t, network.Name, canonical.Name)
		require.NotSame(t, network.Params, canonical.Params)
	}
	require.Equal(t, simNetCoinType, address.SimNetTap.HDCoinType)

	// The tuple selects the network, so a caller cannot rename it and
	// thereby select another proof activation height.
	renamed := CloneAddressParams(address.MainNetTap)
	renamed.Name = chaincfg.RegressionNetParams.Name
	canonical, err := CanonicalAddressParams(renamed)
	require.NoError(t, err)
	require.Equal(t, chaincfg.MainNetParams.Name, canonical.Name)

	canonical.Name = "mutated"
	require.Equal(t, chaincfg.MainNetParams.Name, address.MainNetTap.Name)

	mismatched := CloneAddressParams(address.RegressionNetTap)
	mismatched.TapHRP = address.MainNetTap.TapHRP
	_, err = CanonicalAddressParams(mismatched)
	require.ErrorContains(t, err, "unsupported or mismatched")

	_, err = CanonicalAddressParams(nil)
	require.ErrorContains(t, err, "required")

	_, err = CanonicalAddressParams(&address.ChainParams{
		TapHRP: address.MainNetTap.TapHRP,
	})
	require.ErrorContains(t, err, "incomplete")

	missingHRP := CloneAddressParams(address.MainNetTap)
	missingHRP.TapHRP = ""
	_, err = CanonicalAddressParams(missingHRP)
	require.ErrorContains(t, err, "incomplete")
}

// TestCheckProofFileSize verifies the proof file limit at its boundary.
func TestCheckProofFileSize(t *testing.T) {
	require.NoError(t, checkProofFileSize(make([]byte, 10), 10))
	require.ErrorContains(
		t, checkProofFileSize(make([]byte, 11), 10),
		"exceeds the 10 byte limit",
	)
	require.NoError(t, CheckProofFileSize(nil))
}
