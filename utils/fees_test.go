package utils

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
)

// TestClampSweepFee verifies clamping still respects min relay fee
// requirements and reports clamping.
func TestClampSweepFee(t *testing.T) {
	weight := lntypes.WeightUnit(400)
	minRelay := chainfee.SatPerKWeight(253)
	total := btcutil.Amount(100_000)

	// Fee below the clamp threshold and above min relay should pass
	// through.
	fee := chainfee.SatPerKWeight(1_000).FeeForWeight(weight)
	clamped, clampedFlag, err := ClampSweepFee(
		fee, total, MaxFeeToAmountRatio, minRelay, weight,
	)
	require.NoError(t, err)
	require.Equal(t, fee, clamped)
	require.False(t, clampedFlag)

	// A clamped fee that would fall below min relay should error.
	// The fee will be clamped to 20 sats.
	fee = btcutil.Amount(10_000)
	total = btcutil.Amount(100)
	_, clampedFlag, err = ClampSweepFee(
		fee, total, MaxFeeToAmountRatio, minRelay, weight,
	)
	require.True(t, clampedFlag)
	require.Error(t, err)

	// A fee above the clamp threshold should be clamped without error when
	// still above min relay. The fee is 30% of total, will clamp to 20%.
	fee = btcutil.Amount(30_000)
	total = btcutil.Amount(100_000)
	clamped, clampedFlag, err = ClampSweepFee(
		fee, total, MaxFeeToAmountRatio, minRelay, weight,
	)
	require.NoError(t, err)
	require.True(t, clampedFlag)

	expectedClampedFee := btcutil.Amount(
		float64(total) * MaxFeeToAmountRatio,
	)
	require.Equal(t, expectedClampedFee, clamped)
}
