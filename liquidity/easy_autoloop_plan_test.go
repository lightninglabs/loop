package liquidity

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/swap"
	"github.com/stretchr/testify/require"
)

// TestSuggestEasyAutoloop tests that SuggestSwaps reports what easy autoloop
// would do when it is enabled: the loop out that it would make next, or the
// reason that it would not make one.
func TestSuggestEasyAutoloop(t *testing.T) {
	t.Parallel()

	// Use two channels with a total local balance of 15000, where the
	// first one holds more and is picked first.
	var (
		easyChannel1 = lndclient.ChannelInfo{
			Active:       true,
			ChannelID:    chanID1.ToUint64(),
			PubKeyBytes:  peer1,
			LocalBalance: 9000,
			Capacity:     10000,
		}

		easyChannel2 = lndclient.ChannelInfo{
			Active:       true,
			ChannelID:    chanID2.ToUint64(),
			PubKeyBytes:  peer2,
			LocalBalance: 6000,
			Capacity:     10000,
		}

		inactive1 = easyChannel1
		inactive2 = easyChannel2
	)

	inactive1.Active = false
	inactive2.Active = false

	tests := []struct {
		name string

		// channels is the set of channels that the node has. If it is
		// nil, both active channels are used.
		channels []lndclient.ChannelInfo

		// setParams changes the easy autoloop parameters for the test.
		setParams func(params *Parameters)

		// amount is the amount of the single loop out that we expect.
		// It is zero when we expect no swap.
		amount btcutil.Amount

		// channel is the channel that we expect the loop out to use.
		channel uint64

		// reason is the reason that we expect when there is no swap.
		reason Reason
	}{
		{
			// The excess over the target is swapped out of the
			// channel with the most local balance.
			name:    "swap excess over target",
			amount:  5000,
			channel: chanID1.ToUint64(),
		},
		{
			name: "local balance below target",
			setParams: func(params *Parameters) {
				params.EasyAutoloopTarget = 20000
			},
			reason: ReasonLiquidityOk,
		},
		{
			// The server would allow a swap of 5000, but the client
			// asks for larger swaps.
			name: "excess below client minimum",
			setParams: func(params *Parameters) {
				params.ClientRestrictions.Minimum = 6000
			},
			reason: ReasonBelowMinimum,
		},
		{
			name: "excess clamped to client maximum",
			setParams: func(params *Parameters) {
				params.ClientRestrictions.Maximum = 3000
			},
			amount:  3000,
			channel: chanID1.ToUint64(),
		},
		{
			// A client minimum above the first channel's balance
			// leaves no channel that can make the swap.
			name: "channels below client minimum",
			setParams: func(params *Parameters) {
				params.EasyAutoloopTarget = 5000
				params.ClientRestrictions.Minimum = 9500
			},
			reason: ReasonNoEligibleChannel,
		},
		{
			name: "no active channel",
			channels: []lndclient.ChannelInfo{
				inactive1, inactive2,
			},
			reason: ReasonNoEligibleChannel,
		},
		{
			// The quote's swap fee of 5 sats is more than 0.05% of
			// the swap amount.
			name: "swap over fee limit",
			setParams: func(params *Parameters) {
				params.FeeLimit = &FeePortion{
					PartsPerMillion: 500,
				}
			},
			reason: ReasonSwapFee,
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			cfg, lnd := newTestConfig()

			// The client's limits are applied to the restrictions
			// that the server returns, so each test gets its own.
			cfg.Restrictions = func(context.Context, swap.Type,
				string) (*Restrictions, error) {

				return NewRestrictions(1, 10000), nil
			}

			lnd.Channels = testCase.channels
			if lnd.Channels == nil {
				lnd.Channels = []lndclient.ChannelInfo{
					easyChannel1, easyChannel2,
				}
			}

			// Enable autoloop as well, so that we know that the dry
			// run does not derive an address for the swap.
			params := defaultParameters
			params.AutoloopBudgetLastRefresh = testBudgetStart
			params.Autoloop = true
			params.EasyAutoloop = true
			params.EasyAutoloopTarget = 10000
			params.FeeLimit = defaultFeePortion()

			if testCase.setParams != nil {
				testCase.setParams(&params)
			}

			manager := NewManager(cfg)
			require.NoError(
				t, manager.setParameters(t.Context(), params),
			)

			actual, err := manager.SuggestSwaps(t.Context())
			require.NoError(t, err)

			require.Equal(
				t, testCase.reason, actual.EasyAutoloopReason,
			)
			require.Empty(t, actual.InSwaps)
			require.Empty(t, actual.StaticInSwaps)
			require.Empty(t, actual.DisqualifiedChans)
			require.Empty(t, actual.DisqualifiedPeers)

			if testCase.amount == 0 {
				require.Empty(t, actual.OutSwaps)

				return
			}

			require.Len(t, actual.OutSwaps, 1)

			outSwap := actual.OutSwaps[0]
			require.Equal(t, testCase.amount, outSwap.Amount)
			require.Equal(
				t, loopdb.ChannelSet{testCase.channel},
				outSwap.OutgoingChanSet,
			)
			require.Nil(t, outSwap.DestAddr)
			require.Empty(t, outSwap.Label)

			// The rule based autoloop never gets this swap, which it
			// would dispatch, when easy autoloop is enabled after the
			// autoloop tick chose the rule based path.
			ruleBased, err := manager.suggestSwaps(t.Context(), false)
			require.NoError(t, err)
			require.Empty(t, ruleBased.OutSwaps)
		})
	}
}
