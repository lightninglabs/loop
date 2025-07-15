package deposit

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/test"
	"github.com/stretchr/testify/require"
)

// mockStore is a mock implementation of the Store interface.
type mockStore struct {
	updated bool
}

// AddAssetDeposit is a mock implementation of the AddAssetDeposit method.
func (s *mockStore) AddAssetDeposit(context.Context, *Deposit) error {
	return nil
}

// AddDepositUpdate marks the store as updated.
func (s *mockStore) AddDepositUpdate(context.Context, *Info) error {
	s.updated = true
	return nil
}

// UpdateDeposit is a mock implementation of the UpdateDeposit method.
func (s *mockStore) UpdateDeposit(context.Context, *Deposit) error {
	s.updated = true
	return nil
}

// GetAllDeposits is a mock implementation of the GetAllDeposits method.
func (s *mockStore) GetAllDeposits(context.Context) ([]Deposit, error) {
	return []Deposit{}, nil
}

// testAddDeposit is a helper function that (intrusively) adds a deposit to the
// manager.
func testAddDeposit(t *testing.T, m *Manager, d *Deposit) {
	t.Helper()

	done, err := m.scheduleNextCall()
	require.NoError(t, err)
	defer done()

	m.deposits[d.Info.ID] = d
}

// testUpdateDeposit updates a deposit's state in a race-safe way by using the
// manager's internal synchronization.
func testUpdateDeposit(t *testing.T, m *Manager, d *Deposit, state State,
	outpoint *wire.OutPoint) {

	t.Helper()

	done, err := m.scheduleNextCall()
	require.NoError(t, err)
	defer done()

	d.State = state
	if outpoint != nil {
		d.Outpoint = outpoint
	}

	err = m.handleDepositStateUpdate(m.runCtx(), d)
	require.NoError(t, err)
}

// TestSubscribeDepositUpdates tests the subscription and unsubscription of
// deposit updates.
func TestSubscribeDepositUpdates(t *testing.T) {
	// The test guard ensures that there are no goroutines left running
	// after the test completes.
	defer test.Guard(t)()

	const updateTimeout = 500 * time.Millisecond

	// Create a mock lnd instance to be able to use the chain notifier.
	lnd := test.NewMockLnd()
	store := &mockStore{}

	// Create a new manager with a mock clock and chain notifier.
	m := NewManager(
		nil, nil, nil, lnd.ChainNotifier, nil, store,
		&chaincfg.SimNetParams,
	)

	// We'll use this context for our calls.
	ctxb := context.Background()

	// Start the manager run loop.
	ctxc, cancel := context.WithCancel(ctxb)
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()

		err := m.Run(ctxc, 0)
		require.NoError(t, err)
	}()

	// Create a dummy deposit and add it to the manager.
	depositID := "test_deposit"
	testDeposit := &Deposit{
		Info: &Info{
			ID:    depositID,
			State: StatePending,
		},
	}
	testAddDeposit(t, m, testDeposit)

	// Create a channel to receive deposit updates.
	updateChan := make(chan *Info)

	// Define the update callback.
	updateCallback := func(info *Info) {
		updateChan <- info
	}

	// assertUpdate is a helper function that waits for a deposit update and
	// asserts its state.
	assertUpdate := func(expectedState State) {
		t.Helper()

		select {
		case update := <-updateChan:
			require.Equal(t, depositID, update.ID)
			require.Equal(t, expectedState, update.State)

		case <-time.After(updateTimeout):
			t.Fatalf("expected to receive deposit update with "+
				"state %v", expectedState)
		}
	}

	// Subscribe to deposit updates.
	unsubscribe, err := m.SubscribeDepositUpdates(
		depositID, updateCallback,
	)
	require.NoError(t, err)
	require.NotNil(t, unsubscribe)

	// The subscriber should receive the current deposit info right away.
	assertUpdate(StatePending)

	// Trigger a deposit state update using the helper function.
	testUpdateDeposit(t, m, testDeposit, StateConfirmed, &wire.OutPoint{
		Hash:  chainhash.Hash{1, 2, 3},
		Index: 0,
	})

	// The subscriber should receive the update.
	assertUpdate(StateConfirmed)

	// Now, unsubscribe.
	unsubscribe()

	// Trigger another deposit state update.
	testUpdateDeposit(t, m, testDeposit, StateSwept, nil)

	// The subscriber should not receive any more updates.
	select {
	case <-updateChan:
		t.Fatal("expected not to receive deposit update after " +
			"unsubscribing")

	case <-time.After(updateTimeout):
		// All good, no update received.
	}

	// Stop the manager and wait until it finishes.
	cancel()
	wg.Wait()
}
