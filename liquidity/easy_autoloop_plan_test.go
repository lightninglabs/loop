package liquidity

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/labels"
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

		// pendingAutoloop is an autoloop swap in flight that may pay up
		// to 1000 sats in miner fees, which the budget holds for it.
		pendingAutoloop = &loopdb.LoopOut{
			Contract: &loopdb.LoopOutContract{
				Label:           labels.AutoloopLabel(swap.TypeOut),
				InitiationTime:  testBudgetStart,
				MaxMinerFee:     1000,
				OutgoingChanSet: loopdb.ChannelSet{999},
			},
		}

		// completedAutoloop is an autoloop swap that paid 1000 sats in
		// fees within the budget period.
		completedAutoloop = &loopdb.LoopOut{
			Events: []*loopdb.LoopEvent{{
				Cost:  loopdb.SwapCost{Server: 1000},
				State: loopdb.StateSuccess,
				Time:  testTime,
			}},
			Contract: autoOutContract,
		}
	)

	inactive1.Active = false
	inactive2.Active = false

	tests := []struct {
		name string

		// channels is the set of channels that the node has. If it is
		// nil, both active channels are used.
		channels []lndclient.ChannelInfo

		// loopOuts is the set of loop outs that the client has made.
		loopOuts []*loopdb.LoopOut

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
			// The budget is not spent, but the fees held for a swap
			// in flight fill it.
			name:     "budget held by swap in flight",
			loopOuts: []*loopdb.LoopOut{pendingAutoloop},
			setParams: func(params *Parameters) {
				params.AutoFeeBudget = 500
				params.MaxAutoInFlight = 2
			},
			reason: ReasonBudgetInsufficient,
		},
		{
			name:     "budget spent",
			loopOuts: []*loopdb.LoopOut{completedAutoloop},
			setParams: func(params *Parameters) {
				params.AutoFeeBudget = 500
			},
			reason: ReasonBudgetElapsed,
		},
		{
			// Some budget is left, but less than the swap could
			// cost.
			name: "swap fee over remaining budget",
			setParams: func(params *Parameters) {
				params.AutoFeeBudget = 50
			},
			reason: ReasonBudgetInsufficient,
		},
		{
			// The budget covers the fee limit of 100 sats for this
			// swap, but not the edge-case miner fee that the swap
			// request allows for.
			name: "swap fee limit within remaining budget",
			setParams: func(params *Parameters) {
				params.AutoFeeBudget = 150
			},
			amount:  5000,
			channel: chanID1.ToUint64(),
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

			cfg.ListLoopOut = func(context.Context) (
				[]*loopdb.LoopOut, error) {

				return testCase.loopOuts, nil
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
