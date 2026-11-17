package utils

import (
	"math"
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

// TestFeeForWeight covers rounding and both product and result overflow.
func TestFeeForWeight(t *testing.T) {
	for _, tc := range []struct {
		name    string
		rate    chainfee.SatPerKWeight
		weight  lntypes.WeightUnit
		want    btcutil.Amount
		wantErr bool
	}{
		{name: "ordinary", rate: 1000, weight: 400, want: 400},
		{name: "round down", rate: 1001, weight: 400, want: 400},
		{name: "zero rate", weight: 400},
		{name: "zero weight", rate: 1000, wantErr: true},
		{name: "negative rate", rate: -1, weight: 400, wantErr: true},
		{
			name: "product exceeds int64", rate: math.MaxInt64,
			weight: 1000, want: math.MaxInt64,
		},
		{
			name: "weight exceeds int64", rate: 1,
			weight: math.MaxUint64, want: math.MaxUint64 / 1000,
		},
		{
			name: "result overflow", rate: math.MaxInt64,
			weight: 1001, wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fee, err := FeeForWeight(tc.rate, tc.weight)
			if tc.wantErr {
				require.Error(t, err)
				require.Zero(t, fee)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, fee)
		})
	}
}
