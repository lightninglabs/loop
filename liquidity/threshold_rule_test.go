package liquidity

import (
	"testing"

	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestShouldSwap tests assessing of a set of balances to determine whether we
// should perform a swap. It does not test swap amounts recommended, because
// we test amount calculation separately.
func TestShouldSwap(t *testing.T) {
	var (
		typeIn  = swap.TypeIn
		typeOut = swap.TypeOut
	)

	tests := []struct {
		name        string
		minIncoming int
		minOutgoing int
		balances    *balances
		swapType    *swap.Type
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
			swapType:    nil,
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
			swapType:    &typeOut,
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
			swapType:    &typeIn,
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
			swapType:    nil,
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, swapType := shouldSwap(
				test.balances, test.minIncoming,
				test.minOutgoing,
			)
			require.Equal(t, test.swapType, swapType)
		})
	}
}

// TestCalculateSwapAmount tests calculation of the amount of our capacity that
// we need to shift to reach our liquidity targets.
func TestCalculateSwapAmount(t *testing.T) {
	tests := []struct {
		name string

		capacity    btcutil.Amount
		deficitSide balanceRequirement
		surplusSide balanceRequirement
		expectedAmt btcutil.Amount
	}{
		{
			// We have enough balance to hit our target between our
			// two ratios.
			// start: 	| 100 out         |
			// end: 	| 45 out | 55 in |
			name:     "can reach midpoint",
			capacity: 100,
			deficitSide: balanceRequirement{
				currentAmount: 0,
				minimumAmount: 30,
			},
			surplusSide: balanceRequirement{
				currentAmount: 100,
				minimumAmount: 20,
			},
			expectedAmt: 55,
		},
		{
			// We have a lot of pending htlcs. If we were to shift
			// to our minimum of 50% inbound, that would unbalance
			// our outbound liquidity. We do not recommend a swap.
			// start: 	| 60 out | 30 pending | 10 in |
			// end: 	| 45 out | 30 pending | 25 in |
			name:     "can't swap",
			capacity: 100,
			deficitSide: balanceRequirement{
				currentAmount: 10,
				minimumAmount: 50,
			},
			surplusSide: balanceRequirement{
				currentAmount: 60,
				minimumAmount: 30,
			},
			expectedAmt: 0,
		},
	}

	for _, testCase := range tests {
		testCase := testCase

		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			amt := calculateSwapAmount(
				testCase.capacity, testCase.deficitSide,
				testCase.surplusSide,
			)
			require.Equal(t, testCase.expectedAmt, amt)
		})
	}
}

// TestGetSwaps tests creation of a slice of recommended swaps using for the
// threshold rule.
func TestGetSwaps(t *testing.T) {
	var (
		chan1 = lnwire.NewShortChanIDFromInt(1)
		chan2 = lnwire.NewShortChanIDFromInt(2)
	)

	tests := []struct {
		name            string
		rule            *ThresholdRule
		channels        []balances
		outRestrictions *Restrictions
		inRestrictions  *Restrictions
		swaps           []SwapRecommendation
		err             error
	}{
		{
			name:     "no capacity",
			rule:     NewThresholdRule(10, 10),
			channels: nil,
			err:      ErrNoCapacity,
		},
		{
			name: "liquidity ok",
			rule: NewThresholdRule(10, 10),
			channels: []balances{
				{
					capacity: 100,
					incoming: 50,
					outgoing: 50,
				},
			},
		},
		{
			name:           "loop in",
			rule:           NewThresholdRule(10, 10),
			inRestrictions: NewRestrictions(10, 100),
			channels: []balances{
				{
					capacity: 100,
					incoming: 100,
					outgoing: 0,
				},
			},
			swaps: []SwapRecommendation{
				&LoopInRecommendation{amount: 50},
			},
		},
		{
			name:            "loop out",
			rule:            NewThresholdRule(10, 10),
			outRestrictions: NewRestrictions(10, 100),
			channels: []balances{
				{
					capacity:  100,
					incoming:  0,
					outgoing:  100,
					channelID: chan1,
				},
				{
					capacity:  100,
					incoming:  0,
					outgoing:  100,
					channelID: chan2,
				},
			},
			swaps: []SwapRecommendation{
				&LoopOutRecommendation{
					amount:  100,
					Channel: chan1,
				},
			},
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			swaps, err := test.rule.getSwaps(
				test.channels, test.outRestrictions,
				test.inRestrictions,
			)

			require.Equal(t, test.err, err)
			if test.err != nil {
				return
			}

			require.Equal(t, test.swaps, swaps)
		})
	}
}
