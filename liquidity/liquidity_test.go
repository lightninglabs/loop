package liquidity

import (
	"context"
	"testing"
	"time"

	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/clock"
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

	prepayFee = ppmToSat(
		defaultMaximumPrepay, defaultPrepayRoutingFeePPM,
	)
	routingFee = ppmToSat(7500, defaultRoutingFeePPM)
	swapFee    = ppmToSat(7500, defaultSwapFeePPM)

	// chan1Rec is the suggested swap for channel 1 when we use chanRule.
	chan1Rec = loop.OutRequest{
		Amount:              7500,
		OutgoingChanSet:     loopdb.ChannelSet{chanID1.ToUint64()},
		MaxPrepayRoutingFee: prepayFee,
		MaxSwapRoutingFee:   routingFee,
		MaxMinerFee:         defaultMaximumMinerFee,
		MaxSwapFee:          swapFee,
		MaxPrepayAmount:     defaultMaximumPrepay,
		SweepConfTarget:     loop.DefaultSweepConfTarget,
	}

	// chan2Rec is the suggested swap for channel 2 when we use chanRule.
	chan2Rec = loop.OutRequest{
		Amount:              7500,
		OutgoingChanSet:     loopdb.ChannelSet{chanID2.ToUint64()},
		MaxPrepayRoutingFee: prepayFee,
		MaxSwapRoutingFee:   routingFee,
		MaxMinerFee:         defaultMaximumMinerFee,
		MaxSwapFee:          swapFee,
		MaxPrepayAmount:     defaultMaximumPrepay,
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
// are in use for in-flight swaps.
func TestRestrictedSuggestions(t *testing.T) {
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

			rules := map[lnwire.ShortChannelID]*ThresholdRule{
				chanID1: chanRule,
				chanID2: chanRule,
			}

			testSuggestSwaps(
				t, cfg, lnd, testCase.channels, rules,
				testCase.expected,
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

			channels := []lndclient.ChannelInfo{
				channel1,
			}

			testSuggestSwaps(
				t, cfg, lnd, channels, testCase.rules,
				testCase.swaps,
			)
		})
	}
}

// testSuggestSwaps tests getting swap suggestions.
func testSuggestSwaps(t *testing.T, cfg *Config, lnd *test.LndMockServices,
	channels []lndclient.ChannelInfo,
	rules map[lnwire.ShortChannelID]*ThresholdRule,
	expected []loop.OutRequest) {

	t.Parallel()

	// Create a mock lnd with the set of channels set in our test case and
	// update our test case lnd to use these channels.
	lnd.Channels = channels

	// Create a new manager, get our current set of parameters and update
	// them to use the rules set by the test.
	manager := NewManager(cfg)

	currentParams := manager.GetParameters()
	currentParams.ChannelRules = rules

	err := manager.SetParameters(currentParams)
	require.NoError(t, err)

	actual, err := manager.SuggestSwaps(context.Background())
	require.NoError(t, err)
	require.Equal(t, expected, actual)
}
