package liquidity

import (
	"testing"

	"github.com/btcsuite/btcutil"
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

// TestLoopOutAmount tests assessing of a set of balances to determine whether
// we should perform a loop out.
func TestLoopOutAmount(t *testing.T) {
	tests := []struct {
		name        string
		minIncoming int
		minOutgoing int
		balances    *balances
		amt         btcutil.Amount
	}{
		{
			name: "insufficient surplus",
			balances: &balances{
				capacity: 100,
				incoming: 20,
				outgoing: 20,
			},
			minOutgoing: 40,
			minIncoming: 40,
			amt:         0,
		},
		{
			name: "loop out",
			balances: &balances{
				capacity: 100,
				incoming: 20,
				outgoing: 80,
			},
			minOutgoing: 20,
			minIncoming: 60,
			amt:         50,
		},
		{
			name: "pending htlcs",
			balances: &balances{
				capacity: 100,
				incoming: 20,
				outgoing: 30,
			},
			minOutgoing: 20,
			minIncoming: 60,
			amt:         0,
		},
		{
			name: "loop in",
			balances: &balances{
				capacity: 100,
				incoming: 50,
				outgoing: 50,
			},
			minOutgoing: 60,
			minIncoming: 30,
			amt:         0,
		},
		{
			name: "liquidity ok",
			balances: &balances{
				capacity: 100,
				incoming: 50,
				outgoing: 50,
			},
			minOutgoing: 40,
			minIncoming: 40,
			amt:         0,
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			amt := loopOutSwapAmount(
				test.balances, test.minIncoming,
				test.minOutgoing,
			)
			require.Equal(t, test.amt, amt)
		})
	}
}

// TestSuggestSwaps tests swap suggestions for the threshold rule. It does not
// many different values because we have separate tests for swap amount
// calculation.
func TestSuggestSwap(t *testing.T) {
	tests := []struct {
		name            string
		rule            *ThresholdRule
		channel         *balances
		outRestrictions *Restrictions
		swap            *LoopOutRecommendation
	}{
		{
			name:            "liquidity ok",
			rule:            NewThresholdRule(10, 10),
			outRestrictions: NewRestrictions(10, 100),
			channel: &balances{
				capacity: 100,
				incoming: 50,
				outgoing: 50,
			},
		},
		{
			name:            "loop out",
			rule:            NewThresholdRule(40, 40),
			outRestrictions: NewRestrictions(10, 100),
			channel: &balances{
				capacity: 100,
				incoming: 0,
				outgoing: 100,
			},
			swap: &LoopOutRecommendation{Amount: 50},
		},
		{
			name:            "amount below minimum",
			rule:            NewThresholdRule(40, 40),
			outRestrictions: NewRestrictions(200, 300),
			channel: &balances{
				capacity: 100,
				incoming: 0,
				outgoing: 100,
			},
			swap: nil,
		},
		{
			name:            "amount above maximum",
			rule:            NewThresholdRule(40, 40),
			outRestrictions: NewRestrictions(10, 20),
			channel: &balances{
				capacity: 100,
				incoming: 0,
				outgoing: 100,
			},
			swap: &LoopOutRecommendation{Amount: 20},
		},
		{
			name:            "loop in",
			rule:            NewThresholdRule(10, 10),
			outRestrictions: NewRestrictions(10, 100),
			channel: &balances{
				capacity: 100,
				incoming: 100,
				outgoing: 0,
			},
			swap: nil,
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			swap := test.rule.suggestSwap(
				test.channel, test.outRestrictions,
			)
			require.Equal(t, test.swap, swap)
		})
	}
}
