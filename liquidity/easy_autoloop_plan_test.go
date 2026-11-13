package liquidity

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/labels"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

// countingWalletKit counts the wallet addresses that are derived through it.
type countingWalletKit struct {
	lndclient.WalletKitClient

	addrs atomic.Int32
}

// NextAddr derives a wallet address and counts it.
func (w *countingWalletKit) NextAddr(ctx context.Context, account string,
	addrType walletrpc.AddressType, change bool) (btcutil.Address, error) {

	w.addrs.Add(1)

	return w.WalletKitClient.NextAddr(ctx, account, addrType, change)
}

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

		// chan1Failed is a swap over the first channel that failed to
		// pay within the failure backoff.
		chan1Failed = &loopdb.LoopOut{
			Events: []*loopdb.LoopEvent{{
				State: loopdb.StateFailOffchainPayments,
				Time:  testTime,
			}},
			Contract: chan1Out,
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

		// activeSticky is the number of sticky loop outs that hold an
		// in-flight slot.
		activeSticky int

		// skipped holds the channels that we expect easy autoloop to
		// skip, with the reason for each.
		skipped map[lnwire.ShortChannelID]Reason

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
			skipped: map[lnwire.ShortChannelID]Reason{
				chanID1: ReasonBelowMinimum,
				chanID2: ReasonBelowMinimum,
			},
		},
		{
			name: "no active channel",
			channels: []lndclient.ChannelInfo{
				inactive1, inactive2,
			},
			reason: ReasonNoEligibleChannel,
			skipped: map[lnwire.ShortChannelID]Reason{
				chanID1: ReasonChannelInactive,
				chanID2: ReasonChannelInactive,
			},
		},
		{
			// The channel that is skipped is reported along with
			// the swap from the next channel.
			name: "peer excluded",
			setParams: func(params *Parameters) {
				params.EasyAutoloopExcludedPeers = []route.Vertex{
					peer1,
				}
			},
			amount:  5000,
			channel: chanID2.ToUint64(),
			skipped: map[lnwire.ShortChannelID]Reason{
				chanID1: ReasonPeerExcluded,
			},
		},
		{
			name:     "channel used by another swap",
			loopOuts: []*loopdb.LoopOut{{Contract: chan1Out}},
			amount:   5000,
			channel:  chanID2.ToUint64(),
			skipped: map[lnwire.ShortChannelID]Reason{
				chanID1: ReasonLoopOut,
			},
		},
		{
			name:     "channel backing off after failure",
			loopOuts: []*loopdb.LoopOut{chan1Failed},
			amount:   5000,
			channel:  chanID2.ToUint64(),
			skipped: map[lnwire.ShortChannelID]Reason{
				chanID1: ReasonFailureBackoff,
			},
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
			// A sticky loop out holds the only slot before its swap
			// is stored.
			name:         "sticky loop out holds last slot",
			activeSticky: 1,
			reason:       ReasonInFlight,
		},
		{
			// The sticky loop out's swap is stored, so it is in both
			// counts, and one slot is still free.
			name:         "sticky loop out counted once",
			loopOuts:     []*loopdb.LoopOut{pendingAutoloop},
			activeSticky: 1,
			setParams: func(params *Parameters) {
				params.MaxAutoInFlight = 2
			},
			amount:  5000,
			channel: chanID1.ToUint64(),
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
			// The budget period is over, so the autoloop tick starts
			// a new one before it plans, and the fees spent in the
			// last one no longer count.
			name:     "budget period over",
			loopOuts: []*loopdb.LoopOut{completedAutoloop},
			setParams: func(params *Parameters) {
				params.AutoFeeBudget = 500
				params.AutoFeeRefreshPeriod = time.Minute
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

			cfg.ListLoopOut = func(context.Context) (
				[]*loopdb.LoopOut, error) {

				return testCase.loopOuts, nil
			}

			// The autoloop tick saves the parameters when it starts
			// a new budget period, and the dry run must not.
			var saves atomic.Int32
			cfg.PutLiquidityParams = func(context.Context,
				[]byte) error {

				saves.Add(1)

				return nil
			}

			wallet := &countingWalletKit{
				WalletKitClient: lnd.WalletKit,
			}
			lnd.WalletKit = wallet

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
			manager.activeStickyLoops = testCase.activeSticky

			actual, err := manager.SuggestSwaps(t.Context())
			require.NoError(t, err)

			// The dry run plans with a new budget period when one
			// is due, but leaves the saved budget start alone.
			require.Zero(t, saves.Load())
			require.Equal(
				t, testBudgetStart,
				manager.params.AutoloopBudgetLastRefresh,
			)

			require.Equal(
				t, testCase.reason, actual.EasyAutoloopReason,
			)
			require.Empty(t, actual.InSwaps)
			require.Empty(t, actual.StaticInSwaps)
			require.Empty(t, actual.DisqualifiedPeers)

			skipped := testCase.skipped
			if skipped == nil {
				skipped = noneDisqualified
			}
			require.Equal(t, skipped, actual.DisqualifiedChans)
			require.Zero(t, wallet.addrs.Load())

			// The autoloop tick, which starts a new budget period
			// first when one is due, plans the same swap or gives
			// the same reason. It derives a wallet address only for
			// a swap that it makes. Like the tick, it does not ask
			// for the channels that were skipped.
			manager.refreshAutoloopBudget(t.Context())
			plan, err := manager.planEasyAutoloopSwap(
				t.Context(), manager.params, nil,
			)
			require.NoError(t, err)
			require.Equal(t, testCase.reason, plan.reason)

			if testCase.amount == 0 {
				require.Nil(t, plan.swap)
				require.Zero(t, wallet.addrs.Load())
			} else {
				require.NotNil(t, plan.swap)
				require.Equal(t, testCase.amount, plan.swap.Amount)
				require.Equal(
					t, loopdb.ChannelSet{testCase.channel},
					plan.swap.OutgoingChanSet,
				)
				require.NotNil(t, plan.swap.DestAddr)
				require.Equal(
					t, labels.EasyAutoloopLabel(swap.TypeOut),
					plan.swap.Label,
				)
				require.EqualValues(t, 1, wallet.addrs.Load())
			}

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

// TestSuggestEasyAutoloopDuringRefresh tests that the easy autoloop dry run can
// run while the autoloop tick starts a new budget period, so that the race
// detector checks the budget start that both of them use.
func TestSuggestEasyAutoloopDuringRefresh(t *testing.T) {
	t.Parallel()

	cfg, _ := newTestConfig()
	cfg.PutLiquidityParams = func(context.Context, []byte) error {
		return nil
	}

	// The budget period is over, so the tick starts a new one.
	params := defaultParameters
	params.AutoloopBudgetLastRefresh = testBudgetStart
	params.AutoFeeRefreshPeriod = time.Minute
	params.EasyAutoloop = true
	params.EasyAutoloopTarget = 10000

	manager := NewManager(cfg)
	require.NoError(t, manager.setParameters(t.Context(), params))

	errs := make(chan error, 1)
	go func() {
		_, err := manager.SuggestSwaps(t.Context())
		errs <- err
	}()

	manager.refreshAutoloopBudget(t.Context())
	require.NoError(t, <-errs)

	require.Equal(
		t, cfg.Clock.Now(),
		manager.GetParameters().AutoloopBudgetLastRefresh,
	)
}

// TestSuggestEasyAutoloopStickyWorker tests that easy autoloop waits while a
// sticky loop out holds the only in-flight slot before its swap is stored, and
// plans a swap again once the slot is free.
func TestSuggestEasyAutoloopStickyWorker(t *testing.T) {
	t.Parallel()

	cfg, lnd := newTestConfig()

	// The worker's swap is held inside LoopOut until it is released, or
	// the test ends, and then fails, so that the worker stops and frees
	// its slot.
	entered := make(chan struct{})
	release := make(chan struct{})
	cfg.LoopOut = func(ctx context.Context, _ *loop.OutRequest) (
		*loop.LoopOutSwapInfo, error) {

		close(entered)

		select {
		case <-release:
		case <-ctx.Done():
		}

		return nil, errors.New("swap failed")
	}

	lnd.Channels = []lndclient.ChannelInfo{{
		Active:       true,
		ChannelID:    chanID1.ToUint64(),
		PubKeyBytes:  peer1,
		LocalBalance: 9000,
		Capacity:     10000,
	}}

	params := defaultParameters
	params.AutoloopBudgetLastRefresh = testBudgetStart
	params.EasyAutoloop = true
	params.EasyAutoloopTarget = 5000
	params.FeeLimit = defaultFeePortion()
	params.MaxAutoInFlight = 1

	manager := NewManager(cfg)
	require.NoError(t, manager.setParameters(t.Context(), params))

	done := make(chan struct{})
	go func() {
		defer close(done)

		manager.dispatchStickyLoopOut(
			t.Context(), loop.OutRequest{Amount: 4000}, 1,
			defaultAmountBackoff,
		)
	}()

	select {
	case <-entered:
	case <-done:
		t.Fatal("sticky loop out stopped before dispatching its swap")
	}

	// The dry run and the tick both wait for the slot.
	actual, err := manager.SuggestSwaps(t.Context())
	require.NoError(t, err)
	require.Equal(t, ReasonInFlight, actual.EasyAutoloopReason)
	require.Empty(t, actual.OutSwaps)

	plan, err := manager.planEasyAutoloopSwap(
		t.Context(), manager.params, nil,
	)
	require.NoError(t, err)
	require.Equal(t, ReasonInFlight, plan.reason)
	require.Nil(t, plan.swap)

	close(release)
	<-done

	actual, err = manager.SuggestSwaps(t.Context())
	require.NoError(t, err)
	require.Equal(t, ReasonNone, actual.EasyAutoloopReason)
	require.Len(t, actual.OutSwaps, 1)
}
