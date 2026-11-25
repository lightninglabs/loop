package deposit

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/staticaddr/script"
)

const (
	// expiryTestConfHeight is the confirmation height of the test deposit.
	expiryTestConfHeight = 100

	// expiryTestExpiry is the CSV delay of the test deposit's address.
	expiryTestExpiry = 10

	// expiryTestExpiryHeight is the first height at which the test
	// deposit counts as expired.
	expiryTestExpiryHeight = expiryTestConfHeight + expiryTestExpiry
)

// newExpiryTestFSM returns a deposit state machine in the given state whose
// expiry sweep publication is replaced by a counter. The real transition
// table and block handler are used, so the test observes when the state
// machine decides to publish, not how the sweep is built.
func newExpiryTestFSM(t *testing.T, state fsm.StateType) (*FSM, *Deposit,
	*atomic.Int32) {

	t.Helper()

	deposit := &Deposit{
		Hash:               chainhash.Hash{1},
		ConfirmationHeight: expiryTestConfHeight,
	}
	deposit.SetState(state)

	store := new(mockStore)
	store.On("UpdateDeposit", mock.Anything, mock.Anything).Return(nil)

	depositFSM := &FSM{
		cfg:                  &ManagerConfig{Store: store},
		deposit:              deposit,
		params:               &script.Parameters{Expiry: expiryTestExpiry},
		quitChan:             make(chan struct{}),
		finalizedDepositChan: make(chan wire.OutPoint, 1),
	}

	var published atomic.Int32
	states := depositFSM.DepositStatesV0()

	publish := states[PublishExpirySweep]
	publish.Action = func(context.Context,
		fsm.EventContext) fsm.EventType {

		published.Add(1)

		return OnExpiryPublished
	}
	states[PublishExpirySweep] = publish

	wait := states[WaitForExpirySweep]
	wait.Action = fsm.NoOpAction
	states[WaitForExpirySweep] = wait

	depositFSM.StateMachine = fsm.NewStateMachineWithState(
		states, state, DefaultObserverSize,
	)
	depositFSM.ActionEntryFunc = depositFSM.updateDeposit

	return depositFSM, deposit, &published
}

// requireNoExpirySweep asserts that the deposit stays in the given state and
// that no expiry sweep is published. Block notifications send their event
// from a goroutine, so the assertion has to observe a time window.
func requireNoExpirySweep(t *testing.T, deposit *Deposit,
	state fsm.StateType, published *atomic.Int32) {

	t.Helper()

	require.Never(t, func() bool {
		return deposit.GetState() != state || published.Load() != 0
	}, 150*time.Millisecond, 5*time.Millisecond)
}

// TestExpiryWhileLockedSweepsAfterRelease verifies the client's timeout exit
// for a deposit that expires while a loop-in, a withdrawal or a channel open
// holds it. The owner must not lose the deposit to an expiry sweep while it
// is still working with it. Once the owner gives the deposit back, the next
// block has to start the expiry sweep, even though the expiry height passed
// long before. Otherwise the funds would stay at the static address with the
// timeout path open but never used.
func TestExpiryWhileLockedSweepsAfterRelease(t *testing.T) {
	t.Parallel()

	for _, lockState := range []fsm.StateType{
		LoopingIn, Withdrawing, OpeningChannel,
	} {
		t.Run(string(lockState), func(t *testing.T) {
			t.Parallel()

			depositFSM, deposit, published := newExpiryTestFSM(
				t, lockState,
			)
			ctx := t.Context()

			// Blocks at and after the expiry height reach the locked
			// deposit. The expiry is absorbed every time.
			for _, height := range []uint32{
				expiryTestExpiryHeight - 1,
				expiryTestExpiryHeight,
				expiryTestExpiryHeight + 1,
				expiryTestExpiryHeight + 500,
			} {
				depositFSM.handleBlockNotification(ctx, height)
			}
			requireNoExpirySweep(t, deposit, lockState, published)

			// The owner releases the deposit the way the deposit
			// manager does it, with the deposit lock held.
			deposit.Lock()
			err := depositFSM.SendEvent(ctx, fsm.OnError, nil)
			deposit.Unlock()
			require.NoError(t, err)

			// Releasing alone publishes nothing. The expiry is only
			// evaluated on block notifications.
			requireNoExpirySweep(t, deposit, Deposited, published)

			// The next block starts the expiry sweep.
			depositFSM.handleBlockNotification(
				ctx, expiryTestExpiryHeight+501,
			)
			require.Eventually(t, func() bool {
				return deposit.IsInState(WaitForExpirySweep)
			}, time.Second, 5*time.Millisecond)
			require.EqualValues(t, 1, published.Load())
		})
	}
}

// TestExpiryHeightBoundary verifies that an unlocked deposit is left alone
// one block before its expiry height and swept at the expiry height.
func TestExpiryHeightBoundary(t *testing.T) {
	t.Parallel()

	depositFSM, deposit, published := newExpiryTestFSM(t, Deposited)
	ctx := t.Context()

	depositFSM.handleBlockNotification(ctx, expiryTestExpiryHeight-1)
	requireNoExpirySweep(t, deposit, Deposited, published)

	depositFSM.handleBlockNotification(ctx, expiryTestExpiryHeight)
	require.Eventually(t, func() bool {
		return deposit.IsInState(WaitForExpirySweep)
	}, time.Second, 5*time.Millisecond)
	require.EqualValues(t, 1, published.Load())
}

// TestUnconfirmedDepositNeverExpires verifies that a deposit without a
// confirmation height is not swept, whatever the block height is. Its CSV
// delay has not started yet, so a sweep would be invalid.
func TestUnconfirmedDepositNeverExpires(t *testing.T) {
	t.Parallel()

	depositFSM, deposit, published := newExpiryTestFSM(t, Deposited)
	deposit.ConfirmationHeight = 0

	depositFSM.handleBlockNotification(
		t.Context(), expiryTestExpiryHeight+1_000,
	)
	requireNoExpirySweep(t, deposit, Deposited, published)
}

// TestSweepHtlcTimeoutAbsorbsExpiry verifies that a deposit whose HTLC was
// published without payment is never handed to the expiry sweep. The
// deposit outpoint is already spent by the HTLC transaction, and the loop-in
// recovers the funds through the HTLC timeout path.
func TestSweepHtlcTimeoutAbsorbsExpiry(t *testing.T) {
	t.Parallel()

	depositFSM, deposit, published := newExpiryTestFSM(t, SweepHtlcTimeout)

	depositFSM.handleBlockNotification(
		t.Context(), expiryTestExpiryHeight+1,
	)
	requireNoExpirySweep(t, deposit, SweepHtlcTimeout, published)

	// No release exists for this state. An error event must not move the
	// deposit back to the deposited state, where it would be swept.
	deposit.Lock()
	err := depositFSM.SendEvent(t.Context(), fsm.OnError, nil)
	deposit.Unlock()
	require.Error(t, err)
	requireNoExpirySweep(t, deposit, SweepHtlcTimeout, published)
}
