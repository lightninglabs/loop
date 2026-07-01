package deposit

import (
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/staticaddr/script"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// TestHandleBlockNotificationIgnoresFinalStates verifies that a block-driven
// expiry notification cannot mutate deposits that already reached a final
// state but have not yet been removed from the manager's active set.
func TestHandleBlockNotificationIgnoresFinalStates(t *testing.T) {
	t.Parallel()

	finalStates := []fsm.StateType{
		Expired,
		Withdrawn,
		LoopedIn,
		HtlcTimeoutSwept,
		ChannelPublished,
	}

	for i, state := range finalStates {
		t.Run(string(state), func(t *testing.T) {
			t.Parallel()

			outpoint := wire.OutPoint{
				Hash:  chainhash.Hash{byte(i + 1)},
				Index: uint32(i),
			}
			deposit := &Deposit{
				OutPoint:           outpoint,
				ConfirmationHeight: 1,
			}
			deposit.SetState(state)

			depositFSM := &FSM{
				cfg: &ManagerConfig{
					Store: new(mockStore),
				},
				deposit:              deposit,
				params:               &script.Parameters{Expiry: 1},
				quitChan:             make(chan struct{}),
				finalizedDepositChan: make(chan wire.OutPoint, 1),
			}
			depositFSM.StateMachine = fsm.NewStateMachineWithState(
				depositFSM.DepositStatesV0(), state,
				DefaultObserverSize,
			)
			depositFSM.ActionEntryFunc = depositFSM.updateDeposit

			depositFSM.handleBlockNotification(t.Context(), 3)

			require.Never(t, func() bool {
				return deposit.GetState() != state
			}, 100*time.Millisecond, 10*time.Millisecond)

			select {
			case finalized := <-depositFSM.finalizedDepositChan:
				t.Fatalf("unexpected finalization for %v", finalized)

			default:
			}
		})
	}
}

// TestFinalStatesIgnoreQueuedExpiry verifies that a queued OnExpiry event cannot
// overwrite a deposit that already reached a final state.
func TestFinalStatesIgnoreQueuedExpiry(t *testing.T) {
	t.Parallel()

	finalStates := []fsm.StateType{
		Expired,
		Withdrawn,
		LoopedIn,
		HtlcTimeoutSwept,
		ChannelPublished,
	}

	for i, state := range finalStates {
		t.Run(string(state), func(t *testing.T) {
			t.Parallel()

			outpoint := wire.OutPoint{
				Hash:  chainhash.Hash{byte(i + 1)},
				Index: uint32(i),
			}
			deposit := &Deposit{
				OutPoint: outpoint,
			}
			deposit.SetState(state)

			depositFSM := &FSM{
				cfg: &ManagerConfig{
					Store: new(mockStore),
				},
				deposit:              deposit,
				quitChan:             make(chan struct{}),
				finalizedDepositChan: make(chan wire.OutPoint, 1),
			}
			depositFSM.StateMachine = fsm.NewStateMachineWithState(
				depositFSM.DepositStatesV0(), state,
				DefaultObserverSize,
			)
			depositFSM.ActionEntryFunc = depositFSM.updateDeposit

			err := depositFSM.SendEvent(t.Context(), OnExpiry, nil)
			require.NoError(t, err)
			require.Equal(t, state, deposit.GetState())
		})
	}
}

// TestLoopingInTransitionsToSweepHtlcTimeout verifies that a deposit selected
// by a loop-in can be moved into the timeout sweep state if the server confirms
// the HTLC without paying the invoice.
func TestLoopingInTransitionsToSweepHtlcTimeout(t *testing.T) {
	t.Parallel()

	outpoint := wire.OutPoint{
		Hash:  chainhash.Hash{9},
		Index: 9,
	}
	deposit := &Deposit{
		OutPoint: outpoint,
	}
	deposit.SetState(LoopingIn)

	store := new(mockStore)
	store.On(
		"UpdateDeposit", mock.Anything, mock.Anything,
	).Return(nil).Once()

	depositFSM := &FSM{
		cfg: &ManagerConfig{
			Store: store,
		},
		deposit: deposit,
		params:  &script.Parameters{Expiry: 1},
	}
	depositFSM.StateMachine = fsm.NewStateMachineWithState(
		depositFSM.DepositStatesV0(), LoopingIn, DefaultObserverSize,
	)
	depositFSM.ActionEntryFunc = depositFSM.updateDeposit

	err := depositFSM.SendEvent(
		t.Context(), OnSweepingHtlcTimeout, nil,
	)
	require.NoError(t, err)
	require.Equal(t, SweepHtlcTimeout, deposit.GetState())
	store.AssertExpectations(t)
}
