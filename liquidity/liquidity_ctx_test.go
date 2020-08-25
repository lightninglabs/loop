package liquidity

import (
	"context"
	"testing"

	"github.com/btcsuite/btcutil"
	"github.com/stretchr/testify/require"
)

// testRestrictions is the set of server side restrictions we set for swaps.
var testRestrictions = NewRestrictions(
	btcutil.Amount(1), btcutil.Amount(100000),
)

// newTestConfig creates a default test config.
func newTestConfig() *Config {
	return &Config{
		LoopOutRestrictions: func(_ context.Context) (*Restrictions,
			error) {

			return testRestrictions, nil
		},
		LoopInRestrictions: func(_ context.Context) (*Restrictions,
			error) {

			return testRestrictions, nil
		},
	}
}

type testContext struct {
	t *testing.T

	// manager is an instance of our manager that we are running for this
	// test.
	manager *Manager

	// errChan is a channel that our manager's execution error (if any) will
	// be sent on.
	errChan chan error

	// stop shuts down our test manager.
	stop func()
}

// newTestContext creates a new test context.
func newTestContext(t *testing.T) *testContext {
	cfg := newTestConfig()

	return &testContext{
		t:       t,
		manager: NewManager(cfg),
		errChan: make(chan error),
	}
}

// run starts our test's manager (in a goroutine, because it blocks) and sends
// the error it returns into our test's error chan.
func (t *testContext) run() {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		t.errChan <- t.manager.Run(ctx)
	}()

	t.stop = cancel
}

// waitForFinished stops our manager and asserts that we exit with a context
// cancelled error.
func (t *testContext) waitForFinished() {
	t.stop()
	require.Equal(t.t, context.Canceled, <-t.errChan)
}
