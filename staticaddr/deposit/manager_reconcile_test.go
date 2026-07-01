package deposit

import (
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/stretchr/testify/require"
)

// TestAllOutpointsActiveDepositsRejectsDuplicateOutpoints verifies that a
// duplicated selection is rejected before the manager tries to lock the same
// deposit twice.
func TestAllOutpointsActiveDepositsRejectsDuplicateOutpoints(t *testing.T) {
	outpoint := wire.OutPoint{
		Hash:  chainhash.Hash{12},
		Index: 6,
	}

	deposit := &Deposit{
		OutPoint: outpoint,
	}
	deposit.SetState(Deposited)

	manager := NewManager(&ManagerConfig{})
	manager.deposits[outpoint] = deposit
	manager.activeDeposits[outpoint] = &FSM{
		deposit: deposit,
	}

	deposits, ok := manager.AllOutpointsActiveDeposits(
		[]wire.OutPoint{outpoint, outpoint}, Deposited,
	)
	require.False(t, ok)
	require.Nil(t, deposits)
}

// TestTransitionDepositsRejectsDuplicateOutpoints verifies that transition
// callers cannot deadlock the manager by passing the same deposit twice.
func TestTransitionDepositsRejectsDuplicateOutpoints(t *testing.T) {
	outpoint := wire.OutPoint{
		Hash:  chainhash.Hash{13},
		Index: 6,
	}

	deposit := &Deposit{
		OutPoint: outpoint,
	}
	deposit.SetState(Deposited)

	manager := NewManager(&ManagerConfig{})
	err := manager.TransitionDeposits(
		t.Context(), []*Deposit{deposit, deposit}, OnLoopInInitiated,
		LoopingIn,
	)
	require.ErrorContains(t, err, "duplicate deposit outpoint")
	require.Equal(t, Deposited, deposit.GetState())
}
