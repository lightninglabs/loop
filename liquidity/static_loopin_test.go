package liquidity

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/loop/labels"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
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
