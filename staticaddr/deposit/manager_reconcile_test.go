package deposit

import (
	"testing"
	"time"

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

// TestLockDepositsCanonicalizesOutpoints verifies that lockDeposits takes a
// canonical copy of the caller's slice so overlapping multi-deposit operations
// cannot lock deposits in conflicting request orders.
func TestLockDepositsCanonicalizesOutpoints(t *testing.T) {
	depositA := &Deposit{
		OutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{1},
			Index: 0,
		},
	}
	depositB := &Deposit{
		OutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{2},
			Index: 0,
		},
	}

	deposits := []*Deposit{depositB, depositA}
	lockedDeposits := lockDeposits(deposits)
	defer unlockDeposits(lockedDeposits)

	require.Equal(t, []*Deposit{depositA, depositB}, lockedDeposits)
	require.Equal(t, []*Deposit{depositB, depositA}, deposits)
}

// TestLockDepositsAllowsReversedConcurrentRequests exercises the reviewer
// case where overlapping callers request the same deposits in opposite orders.
func TestLockDepositsAllowsReversedConcurrentRequests(t *testing.T) {
	depositA := &Deposit{
		OutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{3},
			Index: 0,
		},
	}
	depositB := &Deposit{
		OutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{4},
			Index: 0,
		},
	}

	start := make(chan struct{})
	done := make(chan struct{}, 2)
	lockAndUnlock := func(deposits []*Deposit) {
		<-start

		for range 100 {
			lockedDeposits := lockDeposits(deposits)
			unlockDeposits(lockedDeposits)
		}

		done <- struct{}{}
	}

	go lockAndUnlock([]*Deposit{depositA, depositB})
	go lockAndUnlock([]*Deposit{depositB, depositA})

	close(start)

	for range 2 {
		select {
		case <-done:

		case <-time.After(time.Second):
			t.Fatal("reversed deposit lock requests deadlocked")
		}
	}
}
