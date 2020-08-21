package liquidity

import (
	"testing"

	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestSelectSwaps tests selection of swaps from a set of channels.
func TestSelectSwaps(t *testing.T) {
	var (
		chan1 = lnwire.NewShortChanIDFromInt(1)
		chan2 = lnwire.NewShortChanIDFromInt(2)
	)

	tests := []struct {
		name      string
		channels  []channelDetails
		amount    btcutil.Amount
		minAmount btcutil.Amount
		maxAmount btcutil.Amount
		swaps     []SwapRecommendation
	}{
		{
			name: "minimum amount exactly required",
			channels: []channelDetails{
				{
					channel: chan1,
					amount:  10,
				},
				{
					channel: chan2,
					amount:  5,
				},
			},
			amount:    10,
			minAmount: 10,
			maxAmount: 100,
			swaps: []SwapRecommendation{
				&LoopOutRecommendation{
					Channel: chan1,
					amount:  btcutil.Amount(10),
				},
			},
		},
		{
			name: "enough balance, but below minimum",
			channels: []channelDetails{
				{
					channel: chan1,
					amount:  5,
				},
				{
					channel: chan2,
					amount:  5,
				},
			},
			amount:    10,
			minAmount: 10,
			maxAmount: 100,
			swaps:     nil,
		},
		{
			name: "more available than required",
			channels: []channelDetails{
				{
					channel: chan1,
					amount:  50,
				},
			},
			amount:    20,
			minAmount: 10,
			maxAmount: 100,
			swaps: []SwapRecommendation{
				&LoopOutRecommendation{
					Channel: chan1,
					amount:  btcutil.Amount(20),
				},
			},
		},
		{
			name: "more available than required, multiple swaps",
			channels: []channelDetails{
				{
					channel: chan1,
					amount:  200,
				},
				{
					channel: chan2,
					amount:  200,
				},
			},
			amount:    150,
			minAmount: 10,
			maxAmount: 100,
			swaps: []SwapRecommendation{
				&LoopOutRecommendation{
					Channel: chan1,
					amount:  btcutil.Amount(100),
				},
				&LoopOutRecommendation{
					Channel: chan2,
					amount:  btcutil.Amount(50),
				},
			},
		},
		{
			name: "cannot get exact amount",
			channels: []channelDetails{
				{
					channel: chan1,
					amount:  20,
				},
				{
					channel: chan2,
					amount:  20,
				},
			},
			amount:    25,
			minAmount: 10,
			maxAmount: 100,
			swaps: []SwapRecommendation{
				&LoopOutRecommendation{
					Channel: chan1,
					amount:  btcutil.Amount(20),
				},
			},
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			swaps := selectSwaps(
				test.channels, newLoopOutRecommendation,
				test.amount, test.minAmount, test.maxAmount,
			)
			require.Equal(t, test.swaps, swaps)
		})
	}
}
