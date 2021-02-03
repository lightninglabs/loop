package liquidity

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/labels"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

var (
	testTime        = time.Date(2020, 02, 13, 0, 0, 0, 0, time.UTC)
	testBudgetStart = testTime.Add(time.Hour * -1)

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
		Initiator:           autoloopSwapInitiator,
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
		Initiator:           autoloopSwapInitiator,
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

	// autoOutContract is a contract for an existing loop out that was
	// automatically dispatched. This swap is within our test budget period,
	// and restricted to a channel that we do not use in our tests.
	autoOutContract = &loopdb.LoopOutContract{
		SwapContract: loopdb.SwapContract{
			Label:          labels.AutoloopLabel(swap.TypeOut),
			InitiationTime: testBudgetStart,
		},
		OutgoingChanSet: loopdb.ChannelSet{999},
	}

	testRestrictions = NewRestrictions(1, 10000)
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

			return testRestrictions, nil
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

	err := manager.SetParameters(context.Background(), expected)
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
	err = manager.SetParameters(context.Background(), expected)
	require.Equal(t, ErrZeroChannelID, err)
}

// TestValidateRestrictions tests validating client restrictions against a set
// of server restrictions.
func TestValidateRestrictions(t *testing.T) {
	tests := []struct {
		name   string
		client *Restrictions
		server *Restrictions
		err    error
	}{
		{
			name: "client invalid",
			client: &Restrictions{
				Minimum: 100,
				Maximum: 1,
			},
			server: testRestrictions,
			err:    ErrMinimumExceedsMaximumAmt,
		},
		{
			name: "maximum exceeds server",
			client: &Restrictions{
				Maximum: 2000,
			},
			server: &Restrictions{
				Minimum: 1000,
				Maximum: 1500,
			},
			err: ErrMaxExceedsServer,
		},
		{
			name: "minimum less than server",
			client: &Restrictions{
				Minimum: 500,
			},
			server: &Restrictions{
				Minimum: 1000,
				Maximum: 1500,
			},
			err: ErrMinLessThanServer,
		},
	}

	for _, testCase := range tests {
		testCase := testCase

		t.Run(testCase.name, func(t *testing.T) {
			err := validateRestrictions(
				testCase.server, testCase.client,
			)
			require.Equal(t, testCase.err, err)
		})
	}
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
				testCase.expected, nil,
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
				testCase.swaps, nil,
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
				testCase.swaps, nil,
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
				testCase.expected, nil,
			)
		})
	}
}

// TestFeeBudget tests limiting of swap suggestions to a fee budget, with and
// without existing swaps. This test uses example channels and rules which need
// a 7500 sat loop out. With our default parameters, and our test quote with
// a prepay of 500, our total fees are (rounded due to int multiplication):
// swap fee: 1 (as set in test quote)
// route fee: 7500 * 0.005 = 37
// prepay route: 500 * 0.005 = 2 sat
// max miner: set by default params
// Since our routing fees are calculated as a portion of our swap/prepay
// amounts, we use our max miner fee to shift swap cost to values above/below
// our budget, fixing our other fees at 114 sat for simplicity.
func TestFeeBudget(t *testing.T) {
	tests := []struct {
		name string

		// budget is our autoloop budget.
		budget btcutil.Amount

		// maxMinerFee is the maximum miner fee we will pay for swaps.
		maxMinerFee btcutil.Amount

		// existingSwaps represents our existing swaps, mapping their
		// last update time to their total cost.
		existingSwaps map[time.Time]btcutil.Amount

		// expectedSwaps is the set of swaps we expect to be suggested.
		expectedSwaps []loop.OutRequest
	}{
		{
			// Two swaps will cost (78+5000)*2, set exactly 10156
			// budget.
			name:        "budget for 2 swaps, no existing",
			budget:      10156,
			maxMinerFee: 5000,
			expectedSwaps: []loop.OutRequest{
				chan1Rec, chan2Rec,
			},
		},
		{
			// Two swaps will cost (78+5000)*2, set 10155 so we can
			// only afford one swap.
			name:        "budget for 1 swaps, no existing",
			budget:      10155,
			maxMinerFee: 5000,
			expectedSwaps: []loop.OutRequest{
				chan1Rec,
			},
		},
		{
			// Set an existing swap which would limit us to a single
			// swap if it were in our period.
			name:        "existing swaps, before budget period",
			budget:      10156,
			maxMinerFee: 5000,
			existingSwaps: map[time.Time]btcutil.Amount{
				testBudgetStart.Add(time.Hour * -1): 200,
			},
			expectedSwaps: []loop.OutRequest{
				chan1Rec, chan2Rec,
			},
		},
		{
			// Add an existing swap in our budget period such that
			// we only have budget left for one more swap.
			name:        "existing swaps, in budget period",
			budget:      10156,
			maxMinerFee: 5000,
			existingSwaps: map[time.Time]btcutil.Amount{
				testBudgetStart.Add(time.Hour): 500,
			},
			expectedSwaps: []loop.OutRequest{
				chan1Rec,
			},
		},
		{
			name:        "existing swaps, budget used",
			budget:      500,
			maxMinerFee: 1000,
			existingSwaps: map[time.Time]btcutil.Amount{
				testBudgetStart.Add(time.Hour): 500,
			},
			expectedSwaps: nil,
		},
	}

	for _, testCase := range tests {
		testCase := testCase

		t.Run(testCase.name, func(t *testing.T) {
			cfg, lnd := newTestConfig()

			// Create a swap set of existing swaps with our set of
			// existing swap timestamps.
			swaps := make(
				[]*loopdb.LoopOut, 0,
				len(testCase.existingSwaps),
			)

			// Add an event with the timestamp and budget set by
			// our test case.
			for ts, amt := range testCase.existingSwaps {
				event := &loopdb.LoopEvent{
					SwapStateData: loopdb.SwapStateData{
						Cost: loopdb.SwapCost{
							Server: amt,
						},
						State: loopdb.StateSuccess,
					},
					Time: ts,
				}

				swaps = append(swaps, &loopdb.LoopOut{
					Loop: loopdb.Loop{
						Events: []*loopdb.LoopEvent{
							event,
						},
					},
					Contract: autoOutContract,
				})
			}

			cfg.ListLoopOut = func() ([]*loopdb.LoopOut, error) {
				return swaps, nil
			}

			// Set two channels that need swaps.
			lnd.Channels = []lndclient.ChannelInfo{
				channel1,
				channel2,
			}

			params := defaultParameters
			params.ChannelRules = map[lnwire.ShortChannelID]*ThresholdRule{
				chanID1: chanRule,
				chanID2: chanRule,
			}
			params.AutoFeeStartDate = testBudgetStart
			params.AutoFeeBudget = testCase.budget
			params.MaximumMinerFee = testCase.maxMinerFee
			params.MaxAutoInFlight = 2

			// Set our custom max miner fee on each expected swap,
			// rather than having to create multiple vars for
			// different rates.
			for i := range testCase.expectedSwaps {
				testCase.expectedSwaps[i].MaxMinerFee =
					testCase.maxMinerFee
			}

			testSuggestSwaps(
				t, newSuggestSwapsSetup(cfg, lnd, params),
				testCase.expectedSwaps, nil,
			)
		})
	}
}

// TestInFlightLimit tests the limit we place on the number of in-flight swaps
// that are allowed.
func TestInFlightLimit(t *testing.T) {
	tests := []struct {
		name          string
		maxInFlight   int
		existingSwaps []*loopdb.LoopOut
		expectedSwaps []loop.OutRequest
	}{
		{
			name:        "none in flight, extra space",
			maxInFlight: 3,
			expectedSwaps: []loop.OutRequest{
				chan1Rec, chan2Rec,
			},
		},
		{
			name:        "none in flight, exact match",
			maxInFlight: 2,
			expectedSwaps: []loop.OutRequest{
				chan1Rec, chan2Rec,
			},
		},
		{
			name:        "one in flight, one allowed",
			maxInFlight: 2,
			existingSwaps: []*loopdb.LoopOut{
				{
					Contract: autoOutContract,
				},
			},
			expectedSwaps: []loop.OutRequest{
				chan1Rec,
			},
		},
		{
			name:        "max in flight",
			maxInFlight: 1,
			existingSwaps: []*loopdb.LoopOut{
				{
					Contract: autoOutContract,
				},
			},
			expectedSwaps: nil,
		},
		{
			name:        "max swaps exceeded",
			maxInFlight: 1,
			existingSwaps: []*loopdb.LoopOut{
				{
					Contract: autoOutContract,
				},
				{
					Contract: autoOutContract,
				},
			},
			expectedSwaps: nil,
		},
	}

	for _, testCase := range tests {
		testCase := testCase

		t.Run(testCase.name, func(t *testing.T) {
			cfg, lnd := newTestConfig()
			cfg.ListLoopOut = func() ([]*loopdb.LoopOut, error) {
				return testCase.existingSwaps, nil
			}

			lnd.Channels = []lndclient.ChannelInfo{
				channel1, channel2,
			}

			params := defaultParameters
			params.ChannelRules = map[lnwire.ShortChannelID]*ThresholdRule{
				chanID1: chanRule,
				chanID2: chanRule,
			}
			params.MaxAutoInFlight = testCase.maxInFlight

			// By default we only have budget for one swap, increase
			// our budget so that we could recommend more than one
			// swap at a time.
			params.AutoFeeBudget = defaultBudget * 2

			testSuggestSwaps(
				t, newSuggestSwapsSetup(cfg, lnd, params),
				testCase.expectedSwaps, nil,
			)
		})
	}
}

// TestSizeRestrictions tests the use of client-set size restrictions on swaps.
func TestSizeRestrictions(t *testing.T) {
	var (
		serverRestrictions = Restrictions{
			Minimum: 6000,
			Maximum: 10000,
		}

		swap = loop.OutRequest{
			OutgoingChanSet:     loopdb.ChannelSet{chanID1.ToUint64()},
			MaxPrepayRoutingFee: prepayFee,
			MaxMinerFee:         defaultMaximumMinerFee,
			MaxSwapFee:          testQuote.SwapFee,
			MaxPrepayAmount:     testQuote.PrepayAmount,
			SweepConfTarget:     loop.DefaultSweepConfTarget,
			Initiator:           autoloopSwapInitiator,
		}
	)

	tests := []struct {
		name string

		// clientRestrictions holds the restrictions that the client
		// has configured.
		clientRestrictions Restrictions

		// server holds the server's mocked responses to our terms
		// endpoint.
		serverRestrictions []Restrictions

		// expectedAmount is the amount that we expect for our swap.
		expectedAmount btcutil.Amount

		// expectedError is the error we expect.
		expectedError error
	}{
		{
			name: "minimum more than server, swap happens",
			clientRestrictions: Restrictions{
				Minimum: 7000,
			},
			serverRestrictions: []Restrictions{
				serverRestrictions, serverRestrictions,
			},
			expectedAmount: 7500,
		},
		{
			name: "minimum more than server, no swap",
			clientRestrictions: Restrictions{
				Minimum: 8000,
			},
			serverRestrictions: []Restrictions{
				serverRestrictions, serverRestrictions,
			},
			expectedAmount: 0,
		},
		{
			name: "maximum less than server, swap happens",
			clientRestrictions: Restrictions{
				Maximum: 7000,
			},
			serverRestrictions: []Restrictions{
				serverRestrictions, serverRestrictions,
			},
			expectedAmount: 7000,
		},
		{
			// Originally, our client params are ok. But then the
			// server increases its minimum, making the client
			// params stale.
			name: "client params stale over time",
			clientRestrictions: Restrictions{
				Minimum: 6500,
				Maximum: 9000,
			},
			serverRestrictions: []Restrictions{
				serverRestrictions,
				{
					Minimum: 5000,
					Maximum: 6000,
				},
			},
			expectedAmount: 0,
			expectedError:  ErrMaxExceedsServer,
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
			params.ClientRestrictions = testCase.clientRestrictions
			params.ChannelRules = map[lnwire.ShortChannelID]*ThresholdRule{
				chanID1: chanRule,
			}

			// callCount tracks the number of calls we make to
			// our restrictions endpoint.
			var callCount int

			cfg.LoopOutRestrictions = func(_ context.Context) (
				*Restrictions, error) {

				restrictions := testCase.serverRestrictions[callCount]
				callCount++

				return &restrictions, nil
			}

			// If we expect a swap (non-zero amount), we add a
			// swap to our set of expected swaps, and update amount
			// and fee accordingly.
			var expectedSwaps []loop.OutRequest
			if testCase.expectedAmount != 0 {
				swap.Amount = testCase.expectedAmount

				swap.MaxSwapRoutingFee = ppmToSat(
					testCase.expectedAmount,
					defaultRoutingFeePPM,
				)

				expectedSwaps = append(expectedSwaps, swap)
			}

			testSuggestSwaps(
				t, newSuggestSwapsSetup(cfg, lnd, params),
				expectedSwaps, testCase.expectedError,
			)

			require.Equal(
				t, callCount, len(testCase.serverRestrictions),
				"too many restrictions provided by mock",
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
	expected []loop.OutRequest, expectedErr error) {

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

	err := manager.SetParameters(context.Background(), setup.params)
	require.NoError(t, err)

	actual, err := manager.SuggestSwaps(context.Background(), false)
	require.Equal(t, expectedErr, err)
	require.Equal(t, expected, actual)
}
