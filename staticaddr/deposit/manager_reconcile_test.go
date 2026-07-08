package deposit

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/staticaddr/script"
	"github.com/lightninglabs/loop/staticaddr/version"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// TestReconcileDepositsSerialized verifies reconciliation is serialized across
// concurrent callers.
func TestReconcileDepositsSerialized(t *testing.T) {
	ctx := context.Background()
	mockLnd := test.NewMockLnd()
	utxo := &lnwallet.Utxo{
		AddressType:   lnwallet.TaprootPubkey,
		Value:         btcutil.Amount(100_000),
		Confirmations: 0,
		OutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{1},
			Index: 1,
		},
	}

	mockAddressManager := new(mockAddressManager)
	mockAddressManager.On(
		"ListUnspent", mock.Anything, int32(0), int32(MaxConfs),
	).Return([]*lnwallet.Utxo{utxo}, nil)
	mockAddressManager.On(
		"GetStaticAddressParameters", mock.Anything,
	).Return((*script.Parameters)(nil), errors.New("fsm init failed"))

	mockStore := new(mockStore)
	var createCalls atomic.Int32
	createEntered := make(chan struct{})
	releaseCreate := make(chan struct{})
	mockStore.On(
		"CreateDeposit", mock.Anything, mock.Anything,
	).Return(nil).Run(func(mock.Arguments) {
		if createCalls.Add(1) == 1 {
			close(createEntered)
		}

		<-releaseCreate
	})

	manager := NewManager(&ManagerConfig{
		AddressManager: mockAddressManager,
		Store:          mockStore,
		WalletKit:      mockLnd.WalletKit,
		Signer:         mockLnd.Signer,
	})

	var wg sync.WaitGroup
	wg.Add(2)

	errs := make(chan error, 2)
	go func() {
		defer wg.Done()
		errs <- manager.reconcileDeposits(ctx)
	}()

	<-createEntered

	go func() {
		defer wg.Done()
		errs <- manager.reconcileDeposits(ctx)
	}()

	time.Sleep(100 * time.Millisecond)
	close(releaseCreate)
	wg.Wait()
	close(errs)

	var gotErrs []error
	for err := range errs {
		gotErrs = append(gotErrs, err)
	}

	require.EqualValues(t, 1, createCalls.Load())
	require.Len(t, manager.deposits, 1)
	require.Empty(t, manager.activeDeposits)
	require.Len(t, gotErrs, 2)

	var errCount int
	for _, err := range gotErrs {
		if err == nil {
			continue
		}

		errCount++
		require.ErrorContains(t, err, "unable to start new deposit FSM")
	}
	require.Equal(t, 1, errCount)
}

// TestReconcileConfirmedDepositUsesCurrentHeight verifies confirmation heights
// are derived from the manager's current block height.
func TestReconcileConfirmedDepositUsesCurrentHeight(t *testing.T) {
	ctx := context.Background()
	mockLnd := test.NewMockLnd()
	utxo := &lnwallet.Utxo{
		AddressType:   lnwallet.TaprootPubkey,
		Value:         btcutil.Amount(100_000),
		Confirmations: 3,
		OutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{8},
			Index: 1,
		},
	}

	mockAddressManager := new(mockAddressManager)
	mockAddressManager.On(
		"ListUnspent", mock.Anything, int32(0), int32(MaxConfs),
	).Return([]*lnwallet.Utxo{utxo}, nil)
	mockAddressManager.On(
		"GetStaticAddressParameters", mock.Anything,
	).Return((*script.Parameters)(nil), errors.New("fsm init failed"))

	mockStore := new(mockStore)
	mockStore.On(
		"CreateDeposit", mock.Anything, mock.Anything,
	).Return(nil).Run(func(args mock.Arguments) {
		createdDeposit := args.Get(1).(*Deposit)
		require.EqualValues(t, 98, createdDeposit.ConfirmationHeight)
	})

	manager := NewManager(&ManagerConfig{
		AddressManager: mockAddressManager,
		Store:          mockStore,
		WalletKit:      mockLnd.WalletKit,
		Signer:         mockLnd.Signer,
	})
	manager.currentHeight.Store(100)

	err := manager.reconcileDeposits(ctx)
	require.ErrorContains(t, err, "unable to start new deposit FSM")
}

// TestUpdateDepositConfirmationsResetsReorgedDeposit verifies that a deposit
// which remains wallet-visible but loses confirmations has its confirmation
// height reset. This can happen if a confirmed transaction is reorged back into
// the mempool.
func TestUpdateDepositConfirmationsResetsReorgedDeposit(t *testing.T) {
	ctx := context.Background()
	outpoint := wire.OutPoint{
		Hash:  chainhash.Hash{7},
		Index: 2,
	}

	deposit := &Deposit{
		OutPoint:           outpoint,
		ConfirmationHeight: 99,
	}
	deposit.SetState(Deposited)

	utxo := &lnwallet.Utxo{
		OutPoint:      outpoint,
		Confirmations: 0,
	}

	mockStore := new(mockStore)
	mockStore.On(
		"UpdateDeposit", mock.Anything, mock.Anything,
	).Return(nil).Run(func(args mock.Arguments) {
		updatedDeposit := args.Get(1).(*Deposit)
		require.Zero(t, updatedDeposit.ConfirmationHeight)
	})

	manager := NewManager(&ManagerConfig{
		Store: mockStore,
	})
	manager.deposits[outpoint] = deposit

	err := manager.updateDepositConfirmations(ctx, []*lnwallet.Utxo{utxo}, 0)
	require.NoError(t, err)
	require.Zero(t, deposit.ConfirmationHeight)
	mockStore.AssertExpectations(t)
}

// TestUpdateDepositConfirmationsRecomputesPositiveHeight verifies that a
// deposit which is confirmed again at a different height after a reorg does
// not retain its stale, positive confirmation height.
func TestUpdateDepositConfirmationsRecomputesPositiveHeight(t *testing.T) {
	ctx := context.Background()
	outpoint := wire.OutPoint{
		Hash:  chainhash.Hash{10},
		Index: 3,
	}

	deposit := &Deposit{
		OutPoint:           outpoint,
		ConfirmationHeight: 100,
	}
	deposit.SetState(Deposited)

	utxo := &lnwallet.Utxo{
		OutPoint:      outpoint,
		Confirmations: 2,
	}

	mockStore := new(mockStore)
	mockStore.On(
		"UpdateDeposit", mock.Anything, mock.Anything,
	).Return(nil).Run(func(args mock.Arguments) {
		updatedDeposit := args.Get(1).(*Deposit)
		require.EqualValues(t, 109, updatedDeposit.ConfirmationHeight)
	})

	manager := NewManager(&ManagerConfig{
		Store: mockStore,
	})
	manager.deposits[outpoint] = deposit

	err := manager.updateDepositConfirmations(
		ctx, []*lnwallet.Utxo{utxo}, 110,
	)
	require.NoError(t, err)
	require.EqualValues(t, 109, deposit.ConfirmationHeight)
	mockStore.AssertExpectations(t)
}

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

// TestReconcileReplacementDepositCreatesNewDeposit ensures that a replacement
// UTXO is retained as a new deposit while an in-flight deposit remains tied to
// the outpoint selected by a loop-in.
func TestReconcileReplacementDepositCreatesNewDeposit(t *testing.T) {
	ctx := context.Background()
	mockLnd := test.NewMockLnd()
	oldOutpoint := wire.OutPoint{
		Hash:  chainhash.Hash{4},
		Index: 8,
	}
	newOutpoint := wire.OutPoint{
		Hash:  chainhash.Hash{5},
		Index: 9,
	}

	depositID, err := GetRandomDepositID()
	require.NoError(t, err)

	deposit := &Deposit{
		ID:       depositID,
		OutPoint: oldOutpoint,
		Value:    btcutil.Amount(100_000),
	}
	deposit.SetState(LoopingIn)

	utxo := &lnwallet.Utxo{
		OutPoint:      newOutpoint,
		Value:         deposit.Value,
		Confirmations: 0,
	}

	mockAddressManager := new(mockAddressManager)
	mockAddressManager.On(
		"ListUnspent", mock.Anything, int32(0), int32(MaxConfs),
	).Return([]*lnwallet.Utxo{utxo}, nil)
	mockAddressManager.On(
		"GetStaticAddressParameters", mock.Anything,
	).Return(&script.Parameters{
		ProtocolVersion: version.ProtocolVersion_V0,
	}, nil)
	mockAddressManager.On(
		"GetStaticAddress", mock.Anything,
	).Return((*script.StaticAddress)(nil), nil)

	mockStore := new(mockStore)
	var createdDeposit *Deposit
	mockStore.On(
		"CreateDeposit", mock.Anything, mock.Anything,
	).Return(nil).Run(func(args mock.Arguments) {
		createdDeposit = args.Get(1).(*Deposit)
	})

	manager := NewManager(&ManagerConfig{
		AddressManager: mockAddressManager,
		Store:          mockStore,
		WalletKit:      mockLnd.WalletKit,
		Signer:         mockLnd.Signer,
	})
	manager.deposits[oldOutpoint] = deposit
	fsm := &FSM{}
	manager.activeDeposits[oldOutpoint] = fsm

	require.NoError(t, manager.reconcileDeposits(ctx))

	require.Same(t, deposit, manager.deposits[oldOutpoint])
	require.Equal(t, oldOutpoint, deposit.OutPoint)
	require.Equal(t, LoopingIn, deposit.GetState())

	replacement, ok := manager.deposits[newOutpoint]
	require.True(t, ok)
	require.Same(t, createdDeposit, replacement)
	require.NotEqual(t, depositID, replacement.ID)
	require.Equal(t, newOutpoint, replacement.OutPoint)
	require.Equal(t, Deposited, replacement.GetState())
	require.Zero(t, replacement.ConfirmationHeight)

	require.Same(t, fsm, manager.activeDeposits[oldOutpoint])
	require.NotSame(t, fsm, manager.activeDeposits[newOutpoint])

	mockStore.AssertNotCalled(
		t, "UpdateDeposit", mock.Anything, mock.Anything,
	)
}
