package test

import (
	"testing"

	"github.com/lightningnetwork/lnd/zpay32"
	"github.com/stretchr/testify/require"
)

// RequireRouteHintsEqual asserts that two route hint sets are identical.
func RequireRouteHintsEqual(t testing.TB, expected, actual [][]zpay32.HopHint) {
	t.Helper()

	require.Len(t, actual, len(expected))

	for i := range expected {
		require.Len(t, actual[i], len(expected[i]))

		for j := range expected[i] {
			expectedHint := expected[i][j]
			actualHint := actual[i][j]

			require.Equal(
				t, expectedHint.NodeID.SerializeCompressed(),
				actualHint.NodeID.SerializeCompressed(),
			)
			require.Equal(
				t, expectedHint.ChannelID, actualHint.ChannelID,
			)
			require.Equal(
				t, expectedHint.FeeBaseMSat,
				actualHint.FeeBaseMSat,
			)
			require.Equal(
				t, expectedHint.FeeProportionalMillionths,
				actualHint.FeeProportionalMillionths,
			)
			require.Equal(
				t, expectedHint.CLTVExpiryDelta,
				actualHint.CLTVExpiryDelta,
			)
		}
	}
}
