package liquidity

import (
	"context"
	"testing"

	"github.com/lightninglabs/loop/test"
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
