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
	"github.com/stretchr/testify/require"
)

var (
	testTime = time.Date(2020, 02, 13, 0, 0, 0, 0, time.UTC)

	chanID1 = lnwire.NewShortChanIDFromInt(1)
	chanID2 = lnwire.NewShortChanIDFromInt(2)

	channel1 = lndclient.ChannelInfo{
		ChannelID:     chanID1.ToUint64(),
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
