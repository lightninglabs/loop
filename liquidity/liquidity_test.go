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
	// asserts that they are set.
	updateAndAssert := func(params Parameters) {
		err := c.manager.SetParameters(ctx, params)
		require.NoError(t, err)

		newParams, err := c.manager.GetParameters(ctx)
		require.NoError(t, err)
		require.Equal(t, params, *newParams)
	}

	// Create a set of parameters and update our parameters.
	expectCfg := Parameters{
		IncludePrivate: false,
	}
	updateAndAssert(expectCfg)

	// Update a value in our parameters and update them again.
	expectCfg.IncludePrivate = true
	updateAndAssert(expectCfg)

	// Shutdown the manager.
	c.waitForFinished()
}
