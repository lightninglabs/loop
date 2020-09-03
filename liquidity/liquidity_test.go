package liquidity

import (
	"context"
	"testing"

	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// newTestConfig creates a default test config.
func newTestConfig() *Config {
	return &Config{
		LoopOutRestrictions: func(_ context.Context) (*Restrictions,
			error) {

			return NewRestrictions(1, 10000), nil
		},
		Lnd: test.NewMockLnd().Client,
	}
}

// TestParameters tests getting and setting of parameters for our manager.
func TestParameters(t *testing.T) {
	manager := NewManager(newTestConfig())

	chanID := lnwire.NewShortChanIDFromInt(1)

	// Start with the case where we have no rules set.
	startParams := manager.GetParameters()
	require.Equal(t, newParameters(), startParams)

	// Mutate the parameters returned by our get function.
	startParams.ChannelRules[chanID] = NewThresholdRule(1, 1)

	// Make sure that we have not mutated the liquidity manager's params
	// by making this change.
	params := manager.GetParameters()
	require.Equal(t, newParameters(), params)

	// Provide a valid set of parameters and validate assert that they are
	// set.
	originalRule := NewThresholdRule(10, 10)
	expected := Parameters{
		ChannelRules: map[lnwire.ShortChannelID]*ThresholdRule{
			chanID: originalRule,
		},
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

// TestSuggestSwaps tests getting of swap suggestions.
func TestSuggestSwaps(t *testing.T) {
	var (
		chanID1 = lnwire.NewShortChanIDFromInt(1)
		chanID2 = lnwire.NewShortChanIDFromInt(2)
	)

	tests := []struct {
		name       string
		channels   []lndclient.ChannelInfo
		parameters Parameters
		swaps      []*LoopOutRecommendation
	}{
		{
			name:       "no rules",
			channels:   nil,
			parameters: newParameters(),
		},
		{
			name: "loop out",
			channels: []lndclient.ChannelInfo{
				{
					ChannelID:     1,
					Capacity:      1000,
					LocalBalance:  1000,
					RemoteBalance: 0,
				},
			},
			parameters: Parameters{
				ChannelRules: map[lnwire.ShortChannelID]*ThresholdRule{
					chanID1: NewThresholdRule(
						10, 10,
					),
				},
			},
			swaps: []*LoopOutRecommendation{
				{
					Channel: chanID1,
					Amount:  500,
				},
			},
		},
		{
			name: "no rule for channel",
			channels: []lndclient.ChannelInfo{
				{
					ChannelID:     1,
					Capacity:      1000,
					LocalBalance:  0,
					RemoteBalance: 1000,
				},
			},
			parameters: Parameters{
				ChannelRules: map[lnwire.ShortChannelID]*ThresholdRule{
					chanID2: NewThresholdRule(10, 10),
				},
			},
			swaps: nil,
		},
	}

	for _, testCase := range tests {
		testCase := testCase

		t.Run(testCase.name, func(t *testing.T) {
			cfg := newTestConfig()

			// Create a mock lnd with the set of channels set in our
			// test case.
			mock := test.NewMockLnd()
			mock.Channels = testCase.channels
			cfg.Lnd = mock.Client

			manager := NewManager(cfg)

			// Set our test case parameters.
			err := manager.SetParameters(testCase.parameters)
			require.NoError(t, err)

			swaps, err := manager.SuggestSwaps(context.Background())
			require.NoError(t, err)
			require.Equal(t, testCase.swaps, swaps)
		})
	}
}
