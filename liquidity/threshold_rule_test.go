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
				MinimumIncoming: 20,
				MinimumOutgoing: 20,
			},
			err: nil,
		},
		{
			name: "negative incoming",
			threshold: ThresholdRule{
				MinimumIncoming: -1,
				MinimumOutgoing: 20,
			},
			err: errInvalidLiquidityThreshold,
		},
		{
			name: "negative outgoing",
			threshold: ThresholdRule{
				MinimumIncoming: 20,
				MinimumOutgoing: -1,
			},
			err: errInvalidLiquidityThreshold,
		},
		{
			name: "incoming > 1",
			threshold: ThresholdRule{
				MinimumIncoming: 120,
				MinimumOutgoing: 20,
			},
			err: errInvalidLiquidityThreshold,
		},
		{
			name: "outgoing >1",
			threshold: ThresholdRule{
				MinimumIncoming: 20,
				MinimumOutgoing: 120,
			},
			err: errInvalidLiquidityThreshold,
		},
		{
			name: "sum < 100",
			threshold: ThresholdRule{
				MinimumIncoming: 60,
				MinimumOutgoing: 39,
			},
			err: nil,
		},
		{
			name: "sum = 100",
			threshold: ThresholdRule{
				MinimumIncoming: 60,
				MinimumOutgoing: 40,
			},
			err: errInvalidThresholdSum,
		},
		{
			name: "sum > 100",
			threshold: ThresholdRule{
				MinimumIncoming: 60,
				MinimumOutgoing: 60,
			},
			err: errInvalidThresholdSum,
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
