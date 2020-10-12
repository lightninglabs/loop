package liquidity

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

var (
	testTime = time.Date(2020, 02, 13, 0, 0, 0, 0, time.UTC)

	chanID1 = lnwire.NewShortChanIDFromInt(1)
	chanID2 = lnwire.NewShortChanIDFromInt(2)

	peer1 = route.Vertex{1}
	peer2 = route.Vertex{2}

	channel1 = lndclient.ChannelInfo{
		ChannelID:     chanID1.ToUint64(),
		PubKeyBytes:   peer1,
		LocalBalance:  10000,
		RemoteBalance: 0,
		Capacity:      10000,
	}

	channel2 = lndclient.ChannelInfo{
		ChannelID:     chanID2.ToUint64(),
		PubKeyBytes:   peer2,
		LocalBalance:  10000,
		RemoteBalance: 0,
		Capacity:      10000,
	}

	// chanRule is a rule that produces chan1Rec.
	chanRule = NewThresholdRule(50, 0)

	testQuote = &loop.LoopOutQuote{
		SwapFee:      btcutil.Amount(1),
		PrepayAmount: btcutil.Amount(500),
		MinerFee:     btcutil.Amount(50),
	}

	prepayFee = ppmToSat(
		testQuote.PrepayAmount, defaultPrepayRoutingFeePPM,
	)
	routingFee = ppmToSat(7500, defaultRoutingFeePPM)

	// chan1Rec is the suggested swap for channel 1 when we use chanRule.
	chan1Rec = loop.OutRequest{
		Amount:              7500,
		OutgoingChanSet:     loopdb.ChannelSet{chanID1.ToUint64()},
		MaxPrepayRoutingFee: prepayFee,
		MaxSwapRoutingFee:   routingFee,
		MaxMinerFee:         defaultMaximumMinerFee,
		MaxSwapFee:          testQuote.SwapFee,
		MaxPrepayAmount:     testQuote.PrepayAmount,
		SweepConfTarget:     loop.DefaultSweepConfTarget,
	}

	// chan2Rec is the suggested swap for channel 2 when we use chanRule.
	chan2Rec = loop.OutRequest{
		Amount:              7500,
		OutgoingChanSet:     loopdb.ChannelSet{chanID2.ToUint64()},
		MaxPrepayRoutingFee: prepayFee,
		MaxSwapRoutingFee:   routingFee,
		MaxMinerFee:         defaultMaximumMinerFee,
		MaxPrepayAmount:     testQuote.PrepayAmount,
		MaxSwapFee:          testQuote.SwapFee,
		SweepConfTarget:     loop.DefaultSweepConfTarget,
	}

	// chan1Out is a contract that uses channel 1, used to represent on
	// disk swap using chan 1.
	chan1Out = &loopdb.LoopOutContract{
		OutgoingChanSet: loopdb.ChannelSet(
			[]uint64{
				chanID1.ToUint64(),
			},
		),
	}
)

// newTestConfig creates a default test config.
func newTestConfig() (*Config, *test.LndMockServices) {
	lnd := test.NewMockLnd()

	// Set our fee estimate for the default number of confirmations to our
	// limit so that our fees will be ok by default.
	lnd.SetFeeEstimate(
		defaultParameters.SweepConfTarget,
		defaultParameters.SweepFeeRateLimit,
	)

	return &Config{
		LoopOutRestrictions: func(_ context.Context) (*Restrictions,
			error) {

			return NewRestrictions(1, 10000), nil
		},
		Lnd:   &lnd.LndServices,
		Clock: clock.NewTestClock(testTime),
		ListLoopOut: func() ([]*loopdb.LoopOut, error) {
			return nil, nil
		},
		ListLoopIn: func() ([]*loopdb.LoopIn, error) {
			return nil, nil
		},
		LoopOutQuote: func(_ context.Context,
			_ *loop.LoopOutQuoteRequest) (*loop.LoopOutQuote,
			error) {

			return testQuote, nil
		},
	}, lnd
}

// TestParameters tests getting and setting of parameters for our manager.
func TestParameters(t *testing.T) {
	cfg, _ := newTestConfig()
	manager := NewManager(cfg)

	chanID := lnwire.NewShortChanIDFromInt(1)

	// Start with the case where we have no rules set.
	startParams := manager.GetParameters()
	require.Equal(t, defaultParameters, startParams)

	// Mutate the parameters returned by our get function.
	startParams.ChannelRules[chanID] = NewThresholdRule(1, 1)

	// Make sure that we have not mutated the liquidity manager's params
	// by making this change.
	params := manager.GetParameters()
	require.Equal(t, defaultParameters, params)

	// Provide a valid set of parameters and validate assert that they are
	// set.
	originalRule := NewThresholdRule(10, 10)
	expected := defaultParameters
	expected.ChannelRules = map[lnwire.ShortChannelID]*ThresholdRule{
		chanID: originalRule,
	}

	err := manager.SetParameters(expected)
	require.NoError(t, err)

	// Check that changing the parameters we just set does not mutate
	// our liquidity manager's parameters.
	expected.ChannelRules[chanID] = NewThresholdRule(11, 11)

	params = manager.GetParameters()
	require.NoError(t, err)
	require.Equal(t, originalRule, params.ChannelRules[chanID])

	// Set invalid parameters and assert that we fail.
	expected.ChannelRules = map[lnwire.ShortChannelID]*ThresholdRule{
		lnwire.NewShortChanIDFromInt(0): NewThresholdRule(1, 2),
	}
	err = manager.SetParameters(expected)
	require.Equal(t, ErrZeroChannelID, err)
}

// TestRestrictedSuggestions tests getting of swap suggestions when we have
// other in-flight swaps. We setup our manager with a set of channels and rules
// that require a loop out swap, focusing on the filtering our of channels that
// are in use for in-flight swaps, or those which have recently failed.
func TestRestrictedSuggestions(t *testing.T) {
	var (
		failedWithinTimeout = &loopdb.LoopEvent{
			SwapStateData: loopdb.SwapStateData{
				State: loopdb.StateFailOffchainPayments,
			},
			Time: testTime,
		}

		failedBeforeBackoff = &loopdb.LoopEvent{
			SwapStateData: loopdb.SwapStateData{
				State: loopdb.StateFailOffchainPayments,
			},
			Time: testTime.Add(
				defaultFailureBackoff * -1,
			),
		}

		// failedTemporary is a swap that failed outside of our backoff
		// period, but we still want to back off because the swap is
		// considered pending.
		failedTemporary = &loopdb.LoopEvent{
			SwapStateData: loopdb.SwapStateData{
				State: loopdb.StateFailTemporary,
			},
			Time: testTime.Add(
				defaultFailureBackoff * -3,
			),
		}
	)

	tests := []struct {
		name     string
		channels []lndclient.ChannelInfo
		loopOut  []*loopdb.LoopOut
		loopIn   []*loopdb.LoopIn
		expected []loop.OutRequest
	}{
		{
			name: "no existing swaps",
			channels: []lndclient.ChannelInfo{
				channel1,
			},
			loopOut: nil,
			loopIn:  nil,
			expected: []loop.OutRequest{
				chan1Rec,
			},
		},
		{
			name: "unrestricted loop out",
			channels: []lndclient.ChannelInfo{
				channel1, channel2,
			},
			loopOut: []*loopdb.LoopOut{
				{
					Contract: &loopdb.LoopOutContract{
						OutgoingChanSet: nil,
					},
				},
			},
			expected: nil,
		},
		{
			name: "unrestricted loop in",
			channels: []lndclient.ChannelInfo{
				channel1, channel2,
			},
			loopIn: []*loopdb.LoopIn{
				{
					Contract: &loopdb.LoopInContract{
						LastHop: nil,
					},
				},
			},
			expected: nil,
		},
		{
			name: "restricted loop out",
			channels: []lndclient.ChannelInfo{
				channel1, channel2,
			},
			loopOut: []*loopdb.LoopOut{
				{
					Contract: chan1Out,
				},
			},
			expected: []loop.OutRequest{
				chan2Rec,
			},
		},
		{
			name: "restricted loop in",
			channels: []lndclient.ChannelInfo{
				channel1, channel2,
			},
			loopIn: []*loopdb.LoopIn{
				{
					Contract: &loopdb.LoopInContract{
						LastHop: &peer2,
					},
				},
			},
			expected: []loop.OutRequest{
				chan1Rec,
			},
		},
		{
			name: "swap failed recently",
			channels: []lndclient.ChannelInfo{
				channel1,
			},
			loopOut: []*loopdb.LoopOut{
				{
					Contract: chan1Out,
					Loop: loopdb.Loop{
						Events: []*loopdb.LoopEvent{
							failedWithinTimeout,
						},
					},
				},
			},
			expected: nil,
		},
		{
			name: "swap failed before cutoff",
			channels: []lndclient.ChannelInfo{
				channel1,
			},
			loopOut: []*loopdb.LoopOut{
				{
					Contract: chan1Out,
					Loop: loopdb.Loop{
						Events: []*loopdb.LoopEvent{
							failedBeforeBackoff,
						},
					},
				},
			},
			expected: []loop.OutRequest{
				chan1Rec,
			},
		},
		{
			name: "temporary failure",
			channels: []lndclient.ChannelInfo{
				channel1,
			},
			loopOut: []*loopdb.LoopOut{
				{
					Contract: chan1Out,
					Loop: loopdb.Loop{
						Events: []*loopdb.LoopEvent{
							failedTemporary,
						},
					},
				},
			},
			expected: nil,
		},
	}

	for _, testCase := range tests {
		testCase := testCase

		t.Run(testCase.name, func(t *testing.T) {
			// Create a manager config which will return the test
			// case's set of existing swaps.
			cfg, lnd := newTestConfig()
			cfg.ListLoopOut = func() ([]*loopdb.LoopOut, error) {
				return testCase.loopOut, nil
			}
			cfg.ListLoopIn = func() ([]*loopdb.LoopIn, error) {
				return testCase.loopIn, nil
			}

			lnd.Channels = testCase.channels

			params := defaultParameters
			params.ChannelRules = map[lnwire.ShortChannelID]*ThresholdRule{
				chanID1: chanRule,
				chanID2: chanRule,
			}

			testSuggestSwaps(
				t, newSuggestSwapsSetup(cfg, lnd, params),
				testCase.expected,
			)
		})
	}
}

// TestSweepFeeLimit tests getting of swap suggestions when our estimated sweep
// fee is above and below the configured limit.
func TestSweepFeeLimit(t *testing.T) {
	tests := []struct {
		name    string
		feeRate chainfee.SatPerKWeight
		swaps   []loop.OutRequest
	}{
		{
			name:    "fee estimate ok",
			feeRate: defaultSweepFeeRateLimit,
			swaps: []loop.OutRequest{
				chan1Rec,
			},
		},
		{
			name:    "fee estimate above limit",
			feeRate: defaultSweepFeeRateLimit + 1,
			swaps:   nil,
		},
	}

	for _, testCase := range tests {
		testCase := testCase

		t.Run(testCase.name, func(t *testing.T) {
			cfg, lnd := newTestConfig()

			// Set our test case's fee rate for our mock lnd.
			lnd.SetFeeEstimate(
				loop.DefaultSweepConfTarget, testCase.feeRate,
			)

			lnd.Channels = []lndclient.ChannelInfo{
				channel1,
			}

			params := defaultParameters
			params.ChannelRules = map[lnwire.ShortChannelID]*ThresholdRule{
				chanID1: chanRule,
			}

			testSuggestSwaps(
				t, newSuggestSwapsSetup(cfg, lnd, params),
				testCase.swaps,
			)
		})
	}
}

// TestSuggestSwaps tests getting of swap suggestions based on the rules set for
// the liquidity manager and the current set of channel balances.
func TestSuggestSwaps(t *testing.T) {
	tests := []struct {
		name  string
		rules map[lnwire.ShortChannelID]*ThresholdRule
		swaps []loop.OutRequest
	}{
		{
			name:  "no rules",
			rules: map[lnwire.ShortChannelID]*ThresholdRule{},
		},
		{
			name: "loop out",
			rules: map[lnwire.ShortChannelID]*ThresholdRule{
				chanID1: chanRule,
			},
			swaps: []loop.OutRequest{
				chan1Rec,
			},
		},
		{
			name: "no rule for channel",
			rules: map[lnwire.ShortChannelID]*ThresholdRule{
				chanID2: NewThresholdRule(10, 10),
			},
			swaps: nil,
		},
	}

	for _, testCase := range tests {
		testCase := testCase

		t.Run(testCase.name, func(t *testing.T) {
			cfg, lnd := newTestConfig()

			lnd.Channels = []lndclient.ChannelInfo{
				channel1,
			}

			params := defaultParameters
			params.ChannelRules = testCase.rules

			testSuggestSwaps(
				t, newSuggestSwapsSetup(cfg, lnd, params),
				testCase.swaps,
			)
		})
	}
}

// TestFeeLimits tests limiting of swap suggestions by fees.
func TestFeeLimits(t *testing.T) {
	tests := []struct {
		name     string
		quote    *loop.LoopOutQuote
		expected []loop.OutRequest
	}{
		{
			name:  "fees ok",
			quote: testQuote,
			expected: []loop.OutRequest{
				chan1Rec,
			},
		},
		{
			name: "insufficient prepay",
			quote: &loop.LoopOutQuote{
				SwapFee:      1,
				PrepayAmount: defaultMaximumPrepay + 1,
				MinerFee:     50,
			},
		},
		{
			name: "insufficient miner fee",
			quote: &loop.LoopOutQuote{
				SwapFee:      1,
				PrepayAmount: 100,
				MinerFee:     defaultMaximumMinerFee + 1,
			},
		},
		{
			// Swap fee limited to 0.5% of 7500 = 37,5.
			name: "insufficient swap fee",
			quote: &loop.LoopOutQuote{
				SwapFee:      38,
				PrepayAmount: 100,
				MinerFee:     500,
			},
		},
	}

	for _, testCase := range tests {
		testCase := testCase

		t.Run(testCase.name, func(t *testing.T) {
			cfg, lnd := newTestConfig()
			cfg.LoopOutQuote = func(context.Context,
				*loop.LoopOutQuoteRequest) (*loop.LoopOutQuote,
				error) {

				return testCase.quote, nil
			}

			lnd.Channels = []lndclient.ChannelInfo{
				channel1,
			}

			params := defaultParameters
			params.ChannelRules = map[lnwire.ShortChannelID]*ThresholdRule{
				chanID1: chanRule,
			}

			testSuggestSwaps(
				t, newSuggestSwapsSetup(cfg, lnd, params),
				testCase.expected,
			)
		})
	}
}

// testSuggestSwapsSetup contains the elements that are used to create a
// suggest swaps test.
type testSuggestSwapsSetup struct {
	cfg    *Config
	lnd    *test.LndMockServices
	params Parameters
}

// newSuggestSwapsSetup creates a suggest swaps setup struct.
func newSuggestSwapsSetup(cfg *Config, lnd *test.LndMockServices,
	params Parameters) *testSuggestSwapsSetup {

	return &testSuggestSwapsSetup{
		cfg:    cfg,
		lnd:    lnd,
		params: params,
	}
}

// testSuggestSwaps tests getting swap suggestions. It takes a setup struct
// which contains custom setup for the test. If this struct is nil, it will
// use the default parameters and setup two channels (channel1 + channel2) with
// chanRule set for each.
func testSuggestSwaps(t *testing.T, setup *testSuggestSwapsSetup,
	expected []loop.OutRequest) {

	t.Parallel()

	// If our setup struct is nil, we replace it with our default test
	// values.
	if setup == nil {
		cfg, lnd := newTestConfig()

		lnd.Channels = []lndclient.ChannelInfo{
			channel1, channel2,
		}

		params := defaultParameters
		params.ChannelRules = map[lnwire.ShortChannelID]*ThresholdRule{
			chanID1: chanRule,
			chanID2: chanRule,
		}

		setup = &testSuggestSwapsSetup{
			cfg:    cfg,
			lnd:    lnd,
			params: params,
		}
	}

	// Create a new manager, get our current set of parameters and update
	// them to use the rules set by the test.
	manager := NewManager(setup.cfg)

	err := manager.SetParameters(setup.params)
	require.NoError(t, err)

	actual, err := manager.SuggestSwaps(context.Background())
	require.NoError(t, err)
	require.Equal(t, expected, actual)
}
