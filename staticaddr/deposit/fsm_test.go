package deposit

import (
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/staticaddr/script"
	"github.com/lightninglabs/loop/swap"
)

// TestSignDescriptorUsesDepositAddress verifies unilateral signing uses the
// parameters of the address that owns the deposit, without consulting the
// legacy root address. The key locator must be set so that lnd derives the
// signing key directly instead of looking it up by public key.
func TestSignDescriptorUsesDepositAddress(t *testing.T) {
	params := &script.Parameters{
		ClientPubkey: defaultServerPubkey,
		ServerPubkey: defaultServerPubkey,
		Expiry:       144,
		KeyLocator: keychain.KeyLocator{
			Family: keychain.KeyFamily(swap.StaticSingleAddressKeyFamily),
			Index:  0,
		},
		PkScript: []byte{0x51, 0x20, 0x01},
	}
	deposit := &Deposit{
		Value:         100_000,
		AddressParams: params,
	}
	depositFSM := &FSM{deposit: deposit}

	signDesc, err := depositFSM.SignDescriptor(t.Context())
	require.NoError(t, err)

	staticAddress, err := deposit.GetStaticAddressScript()
	require.NoError(t, err)
	require.Equal(t, staticAddress.TimeoutLeaf.Script,
		signDesc.WitnessScript)
	require.Nil(t, signDesc.KeyDesc.PubKey)
	require.Equal(t, params.KeyLocator, signDesc.KeyDesc.KeyLocator)
	require.False(t, signDesc.KeyDesc.KeyLocator.IsEmpty())
	require.EqualValues(t, deposit.Value, signDesc.Output.Value)
	require.Equal(t, params.PkScript, signDesc.Output.PkScript)
}

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

// TestOwnedExpiryDoesNotWaitForDepositLock verifies that an expiry self-loop
// cannot hold the FSM lock while waiting for a deposit lock held by its owner.
func TestOwnedExpiryDoesNotWaitForDepositLock(t *testing.T) {
	for _, test := range []struct {
		state      fsm.StateType
		ownerEvent fsm.EventType
		nextState  fsm.StateType
	}{
		{LoopingIn, OnSweepingHtlcTimeout, SweepHtlcTimeout},
		{SweepHtlcTimeout, OnHtlcTimeoutSwept, HtlcTimeoutSwept},
	} {
		t.Run(string(test.state), func(t *testing.T) {
			deposit := &Deposit{}
			deposit.SetState(test.state)
			store := new(mockStore)
			store.On(
				"UpdateDeposit", mock.Anything, mock.Anything,
			).Return(nil).Once()
			depositFSM := &FSM{
				cfg:                  &ManagerConfig{Store: store},
				deposit:              deposit,
				finalizedDepositChan: make(chan wire.OutPoint, 1),
			}
			depositFSM.StateMachine = fsm.NewStateMachineWithState(
				depositFSM.DepositStatesV0(), test.state,
				DefaultObserverSize,
			)
			depositFSM.ActionEntryFunc = depositFSM.updateDeposit

			// TransitionDeposits holds this lock before sending owner
			// events. Expiry must not take the locks in reverse order.
			deposit.Lock()
			locked := true
			defer func() {
				if locked {
					deposit.Unlock()
				}
			}()
			done := make(chan error, 1)
			go func() {
				done <- depositFSM.SendEvent(t.Context(), OnExpiry, nil)
			}()

			select {
			case err := <-done:
				require.NoError(t, err)
			case <-time.After(time.Second):
				// Release a blocked pre-fix event before failing so the
				// regression leaves no stuck goroutine behind.
				deposit.Unlock()
				locked = false
				select {
				case err := <-done:
					require.NoError(t, err)
				case <-time.After(time.Second):
					t.Fatal("expiry event did not finish after unlocking")
				}
				t.Fatal("expiry self-loop waited for the deposit lock")
			}
			require.Equal(t, test.state, deposit.getStateNoLock())

			// The owner's next event must still update and persist the
			// deposit while its original lock remains held.
			err := depositFSM.SendEvent(t.Context(), test.ownerEvent, nil)
			require.NoError(t, err)
			require.Equal(t, test.nextState, deposit.getStateNoLock())
			deposit.Unlock()
			locked = false
			store.AssertExpectations(t)
		})
	}
}
