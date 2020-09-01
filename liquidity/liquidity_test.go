package liquidity

import (
	"context"
	"testing"

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
