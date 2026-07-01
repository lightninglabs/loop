package deposit

import (
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/staticaddr/script"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

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
