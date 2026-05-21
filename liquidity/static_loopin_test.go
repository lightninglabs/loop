package liquidity

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/labels"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

// TestStaticLoopInHtlcWeight verifies the exact HTLC transaction weight for
// the supported deposit-count and change-shape combinations.
func TestStaticLoopInHtlcWeight(t *testing.T) {
	testCases := []struct {
		name        string
		numDeposits int
		hasChange   bool
		expected    int64
	}{
		{
			name:        "zero deposits no change",
			numDeposits: 0,
			expected:    212,
		},
		{
			name:        "zero deposits with change",
			numDeposits: 0,
			hasChange:   true,
			expected:    384,
		},
		{
			name:        "single deposit no change",
			numDeposits: 1,
			expected:    444,
		},
		{
			name:        "multiple deposits with change",
			numDeposits: 3,
			hasChange:   true,
			expected:    1076,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			weight := staticLoopInHtlcWeight(
				testCase.numDeposits, testCase.hasChange,
			)

			require.Equal(t, testCase.expected, int64(weight))
		})
	}
}

// TestStaticLoopInOnchainFee verifies that the HTLC publish fee and timeout
// sweep fee are combined correctly across the supported shapes.
func TestStaticLoopInOnchainFee(t *testing.T) {
	testCases := []struct {
		name                string
		numDeposits         int
		hasChange           bool
		htlcFeeRate         chainfee.SatPerKWeight
		timeoutSweepFeeRate chainfee.SatPerKWeight
		expected            btcutil.Amount
	}{
		{
			name:     "zero fee rates",
			expected: 0,
		},
		{
			name:                "single deposit without change",
			numDeposits:         1,
			htlcFeeRate:         chainfee.SatPerKWeight(1_200),
			timeoutSweepFeeRate: chainfee.SatPerKWeight(800),
			expected:            884,
		},
		{
			name:                "multiple deposits with change",
			numDeposits:         3,
			hasChange:           true,
			htlcFeeRate:         chainfee.SatPerKWeight(2_500),
			timeoutSweepFeeRate: chainfee.SatPerKWeight(1_700),
			expected:            3439,
		},
		{
			name: "zero htlc fee rate falls back to timeout fee " +
				"rate",
			numDeposits:         2,
			htlcFeeRate:         0,
			timeoutSweepFeeRate: chainfee.SatPerKWeight(1_100),
			expected: chainfee.SatPerKWeight(1_100).FeeForWeight(
				staticLoopInHtlcWeight(2, false),
			) + loopInSweepFee(chainfee.SatPerKWeight(1_100)),
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			onchainFee := staticLoopInOnchainFee(
				testCase.numDeposits, testCase.hasChange,
				testCase.htlcFeeRate,
				testCase.timeoutSweepFeeRate,
			)

			require.Equal(t, testCase.expected, onchainFee)
		})
	}
}

// TestStaticLoopInWorstCaseFees verifies that the helper chooses the larger
// of the cooperative success fee and timeout-path fee.
func TestStaticLoopInWorstCaseFees(t *testing.T) {
	testCases := []struct {
		name                string
		numDeposits         int
		hasChange           bool
		swapFee             btcutil.Amount
		htlcFeeRate         chainfee.SatPerKWeight
		timeoutSweepFeeRate chainfee.SatPerKWeight
		expected            btcutil.Amount
	}{
		{
			name:     "all fees zero",
			expected: 0,
		},
		{
			name:                "returns success fee when larger",
			numDeposits:         1,
			swapFee:             5_000,
			htlcFeeRate:         chainfee.SatPerKWeight(800),
			timeoutSweepFeeRate: chainfee.SatPerKWeight(700),
			expected:            5_000,
		},
		{
			name:                "returns timeout fee when larger",
			numDeposits:         2,
			hasChange:           true,
			swapFee:             1_000,
			htlcFeeRate:         chainfee.SatPerKWeight(5_000),
			timeoutSweepFeeRate: chainfee.SatPerKWeight(3_000),
			expected:            5553,
		},
		{
			name:                "returns equal fee when paths match",
			numDeposits:         1,
			swapFee:             884,
			htlcFeeRate:         chainfee.SatPerKWeight(1_200),
			timeoutSweepFeeRate: chainfee.SatPerKWeight(800),
			expected:            884,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			require.Equal(
				t, testCase.expected,
				staticLoopInWorstCaseFees(
					testCase.numDeposits,
					testCase.hasChange,
					testCase.swapFee, testCase.htlcFeeRate,
					testCase.timeoutSweepFeeRate,
				),
			)
		})
	}
}

// TestCheckExistingAutoLoopsStatic verifies that static autoloops contribute
// to the same budget summary as legacy autoloops.
func TestCheckExistingAutoLoopsStatic(t *testing.T) {
	ctx := t.Context()

	sampleStaticLoopIns := []*StaticLoopInInfo{
		{
			Label:          labels.AutoloopLabel(swap.TypeIn),
			QuotedSwapFee:  50,
			LastUpdateTime: testTime,
		},
		{
			Label:          labels.AutoloopLabel(swap.TypeIn),
			QuotedSwapFee:  80,
			LastUpdateTime: testTime,
			Pending:        true,
			NumDeposits:    2,
		},
		{
			Label:          labels.AutoloopLabel(swap.TypeIn),
			QuotedSwapFee:  70,
			HtlcTxFeeRate:  chainfee.SatPerKWeight(800),
			LastUpdateTime: testTime,
			Failed:         true,
			NumDeposits:    1,
		},
	}

	cfg, _ := newTestConfig()
	cfg.ListStaticLoopIn = func(context.Context) ([]*StaticLoopInInfo,
		error) {

		return sampleStaticLoopIns, nil
	}

	manager := NewManager(cfg)
	params := manager.GetParameters()
	params.AutoloopBudgetLastRefresh = testBudgetStart
	require.NoError(t, manager.setParameters(ctx, params))

	summary, err := manager.checkExistingAutoLoops(ctx, nil, nil)
	require.NoError(t, err)

	require.Equal(t, 1, summary.inFlightCount)
	require.Equal(
		t,
		btcutil.Amount(50)+staticLoopInWorstCaseFees(
			1, false, 70, chainfee.SatPerKWeight(800),
			defaultLoopInSweepFee,
		),
		summary.spentFees,
	)
	require.Equal(
		t,
		staticLoopInWorstCaseFees(
			2, false, 80, 0,
			defaultLoopInSweepFee,
		),
		summary.pendingFees,
	)
}

// TestCurrentSwapTrafficStatic verifies that static loop-ins contribute peer
// blocking and failure backoff information to the shared traffic summary.
func TestCurrentSwapTrafficStatic(t *testing.T) {
	ctx := t.Context()

	cfg, _ := newTestConfig()
	cfg.ListStaticLoopIn = func(context.Context) ([]*StaticLoopInInfo,
		error) {

		return []*StaticLoopInInfo{
			{
				LastHop:        &peer1,
				LastUpdateTime: testTime,
				Pending:        true,
				BlocksLoopIn:   true,
			},
			{
				LastHop:        &peer2,
				LastUpdateTime: testTime,
				Failed:         true,
			},
			{
				LastHop:        &route.Vertex{3},
				LastUpdateTime: testTime,
				Pending:        true,
				BlocksLoopIn:   false,
			},
		}, nil
	}

	manager := NewManager(cfg)
	params := manager.GetParameters()
	params.FailureBackOff = time.Hour
	require.NoError(t, manager.setParameters(ctx, params))

	traffic, err := manager.currentSwapTraffic(ctx, nil, nil)
	require.NoError(t, err)

	require.True(t, traffic.ongoingLoopIn[peer1])
	require.False(t, traffic.ongoingLoopIn[route.Vertex{3}])
	require.Equal(t, testTime, traffic.failedLoopIn[peer2])
}

// TestSuggestSwapsStaticLoopInNoCandidate verifies that the planner surfaces a
// structured disqualification reason when static selection cannot build a
// full-deposit candidate for a peer rule.
func TestSuggestSwapsStaticLoopInNoCandidate(t *testing.T) {
	ctx := t.Context()

	cfg, lnd := newTestConfig()
	cfg.EnableStaticAddressAutoloop = true
	cfg.PrepareStaticLoopIn = func(context.Context, route.Vertex,
		btcutil.Amount, btcutil.Amount, string, string,
		[]string) (*PreparedStaticLoopIn, error) {

		return nil, ErrNoStaticLoopInCandidate
	}

	lnd.Channels = []lndclient.ChannelInfo{
		{
			ChannelID:     lnwire.NewShortChanIDFromInt(10).ToUint64(),
			PubKeyBytes:   peer1,
			LocalBalance:  1_000,
			RemoteBalance: 9_000,
			Capacity:      10_000,
		},
	}

	manager := NewManager(cfg)
	params := manager.GetParameters()
	params.AutoloopBudgetLastRefresh = testBudgetStart
	params.LoopInSource = LoopInSourceStaticAddress
	params.PeerRules = map[route.Vertex]*SwapRule{
		peer1: {
			ThresholdRule: NewThresholdRule(0, 50),
			Type:          swap.TypeIn,
		},
	}
	require.NoError(t, manager.setParameters(ctx, params))

	suggestions, err := manager.SuggestSwaps(ctx)
	require.NoError(t, err)
	require.Empty(t, suggestions.StaticInSwaps)
	require.Equal(
		t, ReasonStaticLoopInNoCandidate,
		suggestions.DisqualifiedPeers[peer1],
	)
}

// TestSuggestSwapsMixedInFlightCount verifies that static loop-ins consume the
// same accepted-suggestion slots as legacy swaps during final filtering.
func TestSuggestSwapsMixedInFlightCount(t *testing.T) {
	ctx := t.Context()

	cfg, lnd := newTestConfig()
	cfg.EnableStaticAddressAutoloop = true
	cfg.PrepareStaticLoopIn = func(_ context.Context, peer route.Vertex,
		_, _ btcutil.Amount, label, initiator string,
		_ []string) (*PreparedStaticLoopIn, error) {

		return &PreparedStaticLoopIn{
			Request: loop.StaticAddressLoopInRequest{
				DepositOutpoints: []string{"static:0"},
				SelectedAmount:   4_000,
				MaxSwapFee:       20,
				LastHop:          &peer,
				Label:            label,
				Initiator:        initiator,
			},
			NumDeposits: 1,
		}, nil
	}

	lnd.Channels = []lndclient.ChannelInfo{
		channel1,
		{
			ChannelID:     lnwire.NewShortChanIDFromInt(20).ToUint64(),
			PubKeyBytes:   peer2,
			LocalBalance:  1_000,
			RemoteBalance: 9_000,
			Capacity:      10_000,
		},
	}

	manager := NewManager(cfg)
	params := manager.GetParameters()
	params.AutoloopBudgetLastRefresh = testBudgetStart
	params.MaxAutoInFlight = 1
	params.FeeLimit = NewFeePortion(500000)
	params.LoopInSource = LoopInSourceStaticAddress
	params.ChannelRules = map[lnwire.ShortChannelID]*SwapRule{
		chanID1: chanRule,
	}
	params.PeerRules = map[route.Vertex]*SwapRule{
		peer2: {
			ThresholdRule: NewThresholdRule(0, 50),
			Type:          swap.TypeIn,
		},
	}
	require.NoError(t, manager.setParameters(ctx, params))

	suggestions, err := manager.SuggestSwaps(ctx)
	require.NoError(t, err)
	require.Len(t, suggestions.OutSwaps, 1)
	require.Empty(t, suggestions.StaticInSwaps)
	require.Equal(t, ReasonInFlight, suggestions.DisqualifiedPeers[peer2])
}

// TestAutoLoopDispatchesStaticLoopIn verifies that the autoloop execution path
// dispatches prepared static loop-ins once they survive final filtering.
func TestAutoLoopDispatchesStaticLoopIn(t *testing.T) {
	ctx := t.Context()

	cfg, lnd := newTestConfig()
	cfg.EnableStaticAddressAutoloop = true

	var (
		prepareCalls     int
		dispatched       *loop.StaticAddressLoopInRequest
		prepareInitiator string
	)

	cfg.PrepareStaticLoopIn = func(_ context.Context, peer route.Vertex,
		minAmount, amount btcutil.Amount, label, initiator string,
		excludedOutpoints []string) (*PreparedStaticLoopIn, error) {

		prepareCalls++
		prepareInitiator = initiator
		require.Equal(t, peer1, peer)
		require.Equal(t, testRestrictions.Minimum, minAmount)
		require.Equal(t, testRestrictions.Maximum, amount)
		require.Empty(t, excludedOutpoints)

		return &PreparedStaticLoopIn{
			Request: loop.StaticAddressLoopInRequest{
				DepositOutpoints: []string{"static:0"},
				SelectedAmount:   testRestrictions.Maximum,
				MaxSwapFee:       100,
				LastHop:          &peer,
				Label:            label,
				Initiator:        initiator,
			},
			NumDeposits: 1,
		}, nil
	}
	cfg.StaticLoopIn = func(_ context.Context,
		request *loop.StaticAddressLoopInRequest) (
		*StaticLoopInDispatchResult, error) {

		requestCopy := *request
		dispatched = &requestCopy

		return &StaticLoopInDispatchResult{
			SwapHash: lntypes.Hash{1},
		}, nil
	}

	lnd.Channels = []lndclient.ChannelInfo{
		{
			ChannelID:     lnwire.NewShortChanIDFromInt(10).ToUint64(),
			PubKeyBytes:   peer1,
			LocalBalance:  0,
			RemoteBalance: 100_000,
			Capacity:      100_000,
		},
	}

	manager := NewManager(cfg)
	params := manager.GetParameters()
	params.Autoloop = true
	params.AutoFeeBudget = 100_000
	params.AutoFeeRefreshPeriod = testBudgetRefresh
	params.AutoloopBudgetLastRefresh = testBudgetStart
	params.MaxAutoInFlight = 1
	params.FailureBackOff = time.Hour
	params.FeeLimit = NewFeePortion(500_000)
	params.LoopInSource = LoopInSourceStaticAddress
	params.PeerRules = map[route.Vertex]*SwapRule{
		peer1: {
			ThresholdRule: NewThresholdRule(0, 60),
			Type:          swap.TypeIn,
		},
	}
	require.NoError(t, manager.setParameters(ctx, params))

	err := manager.autoloop(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, prepareCalls)
	require.Equal(t, autoloopSwapInitiator, prepareInitiator)
	require.NotNil(t, dispatched)
	require.Equal(t, []string{"static:0"}, dispatched.DepositOutpoints)
	require.Equal(t, testRestrictions.Maximum, dispatched.SelectedAmount)
	require.Equal(
		t, labels.AutoloopLabel(swap.TypeIn), dispatched.Label,
	)
	require.Equal(t, autoloopSwapInitiator, dispatched.Initiator)
	require.NotNil(t, dispatched.LastHop)
	require.Equal(t, peer1, *dispatched.LastHop)
}
