package liquidity

import (
	"context"
	"testing"

	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

// TestParameters tests get and set of parameters at runtime.
func TestParameters(t *testing.T) {
	defer test.Guard(t)()

	c := newTestContext(t)
	ctx := context.Background()

	c.run()

	// First, we query with no parameters to check that we get the correct
	// error.
	_, err := c.manager.GetParameters(ctx)
	require.Equal(t, err, ErrNoParameters)

	// updateAndAssert is a helper function which updates our parameters and
	// asserts that they are set if we expect the update to succeed.
	updateAndAssert := func(params Parameters, expectedError error) {
		err := c.manager.SetParameters(ctx, params)
		require.Equal(t, expectedError, err)

		// If we expect an error, we do not need to check our updated
		// value, because the set failed.
		if expectedError != nil {
			return
		}

		newParams, err := c.manager.GetParameters(ctx)
		require.NoError(t, err)
		require.Equal(t, params, *newParams)
	}

	// Create a set of parameters and update our parameters.
	expectCfg := Parameters{
		IncludePrivate: false,
	}
	updateAndAssert(expectCfg, nil)

	// Update a value in our parameters and update them again.
	expectCfg.IncludePrivate = true
	updateAndAssert(expectCfg, nil)

	// Try to set a nop target with a rule.
	invalidCfg := Parameters{
		Target:         TargetNone,
		Rule:           NewThresholdRule(2, 2),
		IncludePrivate: false,
	}
	updateAndAssert(invalidCfg, ErrUnexpectedRule)

	// Try to set a target without a rule.
	invalidCfg.Target = TargetChannel
	invalidCfg.Rule = nil
	updateAndAssert(invalidCfg, ErrNoRule)

	// Try to set a target with an invalid rule.
	invalidCfg.Rule = NewThresholdRule(99, 10)
	updateAndAssert(invalidCfg, ErrInvalidThresholdSum)

	// Finally, update with valid target and rule.
	expectCfg.Target = TargetChannel
	expectCfg.Rule = NewThresholdRule(10, 10)
	updateAndAssert(expectCfg, nil)

	// Shutdown the manager.
	c.waitForFinished()
}

// TestSuggestSwaps tests suggesting swaps for different channel balances and
// liquidity manager targets. It does not test a wise variety of balances,
// because balance calculations are covered in other unit tests.
func TestSuggestSwaps(t *testing.T) {
	var (
		peer1 = route.Vertex{1}
		peer2 = route.Vertex{2}

		chan1 = lnwire.NewShortChanIDFromInt(1)
		chan2 = lnwire.NewShortChanIDFromInt(2)
		chan3 = lnwire.NewShortChanIDFromInt(3)

		channel1 = lndclient.ChannelInfo{
			Capacity:      100,
			LocalBalance:  0,
			RemoteBalance: 100,
			PubKeyBytes:   peer1,
			ChannelID:     chan1.ToUint64(),
		}

		channel2 = lndclient.ChannelInfo{
			Capacity:      100,
			LocalBalance:  100,
			RemoteBalance: 0,
			PubKeyBytes:   peer2,
			ChannelID:     chan2.ToUint64(),
			Private:       true,
		}

		channel3 = lndclient.ChannelInfo{
			Capacity:      100,
			LocalBalance:  50,
			RemoteBalance: 50,
			PubKeyBytes:   peer2,
			ChannelID:     chan3.ToUint64(),
		}
	)

	tests := []struct {
		name     string
		channels []lndclient.ChannelInfo
		params   Parameters
		expected []SwapRecommendation
	}{
		{
			// Select a set of channels that have exactly 50/50
			// split on the node level, we should take no action
			// on the node-level.
			name: "node level, balanced",
			channels: []lndclient.ChannelInfo{
				channel1, channel2,
				channel3,
			},
			params: Parameters{
				Rule:           NewThresholdRule(40, 40),
				Target:         TargetNode,
				IncludePrivate: true,
			},
			expected: nil,
		},
		{
			// Select a set of channels that are not balanced when
			// we exclude private channels.
			name: "node level, unbalanced",
			channels: []lndclient.ChannelInfo{
				channel1, channel2,
				channel3,
			},
			params: Parameters{
				Rule:           NewThresholdRule(40, 40),
				Target:         TargetNode,
				IncludePrivate: false,
			},
			expected: []SwapRecommendation{
				&LoopInRecommendation{
					amount:  50,
					LastHop: peer1,
				},
			},
		},
		{
			// Select a set of channels that have exactly 50/50
			// split on the node level, but are not balanced per
			// peer.
			// Peer 1: (0/100) -> (50/50)
			// Peer 2: (100/0) & (50/50) -> (50/50) & (50/50)
			name: "node level, balanced",
			channels: []lndclient.ChannelInfo{
				channel1, channel2, channel3,
			},
			params: Parameters{
				Rule:           NewThresholdRule(40, 40),
				Target:         TargetPeer,
				IncludePrivate: true,
			},
			expected: []SwapRecommendation{
				&LoopInRecommendation{
					amount:  50,
					LastHop: peer1,
				},
				&LoopOutRecommendation{
					amount:  50,
					Channel: chan2,
				},
			},
		},
		{
			// Select a set of channels that have exactly 50/50
			// split on the node level, but are not balanced per
			// channel.
			// Channel 1: (0/100) -> (50/50)
			// Channel 2: (100/0) -> (50/50)
			// Channel 3: (50/50) -> ok
			name: "node level, balanced",
			channels: []lndclient.ChannelInfo{
				channel1, channel2, channel3,
			},
			params: Parameters{
				Rule:           NewThresholdRule(40, 40),
				Target:         TargetChannel,
				IncludePrivate: true,
			},
			expected: []SwapRecommendation{
				&LoopInRecommendation{
					amount:  50,
					LastHop: peer1,
				},
				&LoopOutRecommendation{
					amount:  50,
					Channel: chan2,
				},
			},
		},
	}

	for _, testCase := range tests {
		testCase := testCase

		t.Run(testCase.name, func(t *testing.T) {
			testSuggestSwap(
				t, testCase.channels, testCase.params,
				testCase.expected,
			)
		})
	}

}

func testSuggestSwap(t *testing.T, channels []lndclient.ChannelInfo,
	params Parameters, expectedSwaps []SwapRecommendation) {

	// Create a test context and overwrite our list channels function to
	// return the set of channels we provide.
	ctx := newTestContext(t)
	ctx.manager.cfg.ListChannels = func(_ context.Context) (
		[]lndclient.ChannelInfo, error) {

		return channels, nil
	}

	// Start our liquidity manager.
	ctx.run()

	// Set our test parameters.
	err := ctx.manager.SetParameters(context.Background(), params)
	require.NoError(t, err)

	// Get swap suggestions and assert that they match our expected values.
	suggestion, err := ctx.manager.SuggestSwap(context.Background())
	require.NoError(t, err)
	require.Equal(t, params.Rule, suggestion.Rule)
	require.Equal(t, params.Target, suggestion.Target)
	require.Equal(t, expectedSwaps, suggestion.Suggestions)

	// Shutdown and assert no error.
	ctx.waitForFinished()
}
