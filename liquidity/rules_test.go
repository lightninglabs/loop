package liquidity

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestValidateThreshold tests validation of the values set for a threshold
// rule.
func TestValidateThreshold(t *testing.T) {
	tests := []struct {
		name      string
		threshold ThresholdRule
		err       error
	}{
		{
			name: "values ok",
			threshold: ThresholdRule{
				MinimumInbound:  20,
				MinimumOutbound: 20,
			},
			err: nil,
		},
		{
			name: "negative inbound",
			threshold: ThresholdRule{
				MinimumInbound:  -1,
				MinimumOutbound: 20,
			},
			err: ErrInvalidLiquidityThreshold,
		},
		{
			name: "negative outbound",
			threshold: ThresholdRule{
				MinimumInbound:  20,
				MinimumOutbound: -1,
			},
			err: ErrInvalidLiquidityThreshold,
		},
		{
			name: "inbound > 1",
			threshold: ThresholdRule{
				MinimumInbound:  120,
				MinimumOutbound: 20,
			},
			err: ErrInvalidLiquidityThreshold,
		},
		{
			name: "outbound >1",
			threshold: ThresholdRule{
				MinimumInbound:  20,
				MinimumOutbound: 120,
			},
			err: ErrInvalidLiquidityThreshold,
		},
		{
			name: "sum < 100",
			threshold: ThresholdRule{
				MinimumInbound:  60,
				MinimumOutbound: 39,
			},
			err: nil,
		},
		{
			name: "sum = 100",
			threshold: ThresholdRule{
				MinimumInbound:  60,
				MinimumOutbound: 40,
			},
			err: ErrInvalidThresholdSum,
		},
		{
			name: "sum > 100",
			threshold: ThresholdRule{
				MinimumInbound:  60,
				MinimumOutbound: 60,
			},
			err: ErrInvalidThresholdSum,
		},
	}

	for _, testCase := range tests {
		testCase := testCase

		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			err := testCase.threshold.validate()
			require.Equal(t, testCase.err, err)
		})
	}
}
