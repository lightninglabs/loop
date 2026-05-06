package deposit

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/staticaddr/address"
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
		"GetParameters", mock.Anything,
	).Return(&address.Parameters{
		ClientPubkey:    defaultServerPubkey,
		ServerPubkey:    defaultServerPubkey,
		Expiry:          defaultExpiry,
		PkScript:        utxo.PkScript,
		ProtocolVersion: 999,
	})

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
		_, err := manager.ReconcileDeposits(ctx)
		errs <- err
	}()

	<-createEntered

	go func() {
		defer wg.Done()
		_, err := manager.ReconcileDeposits(ctx)
		errs <- err
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
		errMsg := err.Error()
		require.True(
			t,
			strings.Contains(
				errMsg, "unable to start new deposit FSM",
			) || strings.Contains(
				errMsg, "unable to sync active deposits",
			),
			"unexpected error: %v", err,
		)
	}
	require.Equal(t, 2, errCount)
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
		"GetParameters", mock.Anything,
	).Return(&address.Parameters{
		ClientPubkey:    defaultServerPubkey,
		ServerPubkey:    defaultServerPubkey,
		Expiry:          defaultExpiry,
		PkScript:        utxo.PkScript,
		ProtocolVersion: 999,
	})

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

	_, err := manager.reconcileDeposits(ctx)
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

// TestReconcileDepositsDeactivatesVanishedUnconfirmedDeposit verifies that a
// missing wallet outpoint is removed from the live active set without mutating
// its historical DB state.
func TestReconcileDepositsDeactivatesVanishedUnconfirmedDeposit(t *testing.T) {
	ctx := context.Background()
	outpoint := wire.OutPoint{
		Hash:  chainhash.Hash{2},
		Index: 7,
	}

	deposit := &Deposit{
		OutPoint: outpoint,
	}
	deposit.SetState(Deposited)

	mockAddressManager := new(mockAddressManager)
	mockAddressManager.On(
		"ListUnspent", mock.Anything, int32(0), int32(MaxConfs),
	).Return([]*lnwallet.Utxo{}, nil)

	manager := NewManager(&ManagerConfig{
		AddressManager: mockAddressManager,
		Store:          new(mockStore),
	})
	manager.deposits[outpoint] = deposit
	fsm := &FSM{
		deposit:  deposit,
		stopChan: make(chan struct{}),
		quitChan: make(chan struct{}),
	}
	go func() {
		<-fsm.stopChan
		close(fsm.quitChan)
	}()
	manager.activeDeposits[outpoint] = fsm

	_, err := manager.reconcileDeposits(ctx)
	require.NoError(t, err)
	require.Equal(t, Deposited, deposit.GetState())
	require.Empty(t, manager.activeDeposits)
	select {
	case <-fsm.quitChan:

	case <-time.After(time.Second):
		t.Fatal("fsm did not stop after deposit vanished")
	}
}

// TestReconcileDepositsDeactivatesVanishedConfirmedDeposit verifies that a
// previously confirmed deposit is also removed from the live active set if it
// vanishes from the wallet view.
func TestReconcileDepositsDeactivatesVanishedConfirmedDeposit(t *testing.T) {
	ctx := context.Background()
	outpoint := wire.OutPoint{
		Hash:  chainhash.Hash{9},
		Index: 4,
	}

	deposit := &Deposit{
		OutPoint:           outpoint,
		ConfirmationHeight: 123,
	}
	deposit.SetState(Deposited)

	mockAddressManager := new(mockAddressManager)
	mockAddressManager.On(
		"ListUnspent", mock.Anything, int32(0), int32(MaxConfs),
	).Return([]*lnwallet.Utxo{}, nil)

	manager := NewManager(&ManagerConfig{
		AddressManager: mockAddressManager,
		Store:          new(mockStore),
	})
	manager.deposits[outpoint] = deposit
	fsm := &FSM{
		deposit:  deposit,
		stopChan: make(chan struct{}),
		quitChan: make(chan struct{}),
	}
	go func() {
		<-fsm.stopChan
		close(fsm.quitChan)
	}()
	manager.activeDeposits[outpoint] = fsm

	_, err := manager.reconcileDeposits(ctx)
	require.NoError(t, err)
	require.Equal(t, Deposited, deposit.GetState())
	require.EqualValues(t, 123, deposit.ConfirmationHeight)
	require.Empty(t, manager.activeDeposits)
	select {
	case <-fsm.quitChan:

	case <-time.After(time.Second):
		t.Fatal("fsm did not stop after confirmed deposit vanished")
	}
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

// TestReconcileDepositsReactivatesReappearedDeposit verifies that the same
// outpoint can become active again if lnd reports it after a prior wallet-view
// miss.
func TestReconcileDepositsReactivatesReappearedDeposit(t *testing.T) {
	ctx := context.Background()
	outpoint := wire.OutPoint{
		Hash:  chainhash.Hash{3},
		Index: 5,
	}

	deposit := &Deposit{
		OutPoint:           outpoint,
		Value:              btcutil.Amount(100_000),
		ConfirmationHeight: 77,
		AddressParams: &address.Parameters{
			ClientPubkey:    defaultServerPubkey,
			ServerPubkey:    defaultServerPubkey,
			Expiry:          defaultExpiry,
			ProtocolVersion: version.ProtocolVersion_V0,
		},
	}
	deposit.SetState(Deposited)

	utxo := &lnwallet.Utxo{
		OutPoint:      outpoint,
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
	var updateStates []fsm.StateType
	mockStore.On(
		"UpdateDeposit", mock.Anything, mock.Anything,
	).Return(nil).Run(func(args mock.Arguments) {
		updatedDeposit := args.Get(1).(*Deposit)
		updateStates = append(updateStates, updatedDeposit.state)
		if updatedDeposit.IsInStateNoLock(Deposited) {
			require.Zero(t, updatedDeposit.ConfirmationHeight)
		}
	})

	manager := NewManager(&ManagerConfig{
		AddressManager: mockAddressManager,
		Store:          mockStore,
	})
	manager.deposits[outpoint] = deposit

	// Reconciliation should reactivate the existing record instead of
	// creating a second deposit entry for the same outpoint.
	_, err := manager.reconcileDeposits(ctx)
	require.NoError(t, err)
	require.Equal(t, Deposited, deposit.GetState())
	require.Zero(t, deposit.ConfirmationHeight)
	require.Len(t, manager.activeDeposits, 1)
	require.Equal(t, []fsm.StateType{Deposited}, updateStates)
}

// TestReconcileDepositsKeepsInactiveOnFSMStartFailure verifies that a failed
// reactivation does not leave memory saying a deposit is active without an FSM.
func TestReconcileDepositsKeepsInactiveOnFSMStartFailure(t *testing.T) {
	ctx := context.Background()
	outpoint := wire.OutPoint{
		Hash:  chainhash.Hash{11},
		Index: 5,
	}

	deposit := &Deposit{
		OutPoint:           outpoint,
		Value:              btcutil.Amount(100_000),
		ConfirmationHeight: 77,
	}
	deposit.SetState(Deposited)

	utxo := &lnwallet.Utxo{
		OutPoint:      outpoint,
		Value:         deposit.Value,
		Confirmations: 0,
	}

	mockAddressManager := new(mockAddressManager)
	mockAddressManager.On(
		"ListUnspent", mock.Anything, int32(0), int32(MaxConfs),
	).Return([]*lnwallet.Utxo{utxo}, nil)
	mockAddressManager.On(
		"GetStaticAddressParameters", mock.Anything,
	).Return((*script.Parameters)(nil), errors.New("fsm init failed"))

	var (
		updateStates  []fsm.StateType
		updateHeights []int64
	)
	mockStore := new(mockStore)
	mockStore.On(
		"UpdateDeposit", mock.Anything, mock.Anything,
	).Return(nil).Run(func(args mock.Arguments) {
		updatedDeposit := args.Get(1).(*Deposit)
		updateStates = append(updateStates, updatedDeposit.state)
		updateHeights = append(
			updateHeights, updatedDeposit.ConfirmationHeight,
		)
	})

	manager := NewManager(&ManagerConfig{
		AddressManager: mockAddressManager,
		Store:          mockStore,
	})
	manager.deposits[outpoint] = deposit

	_, err := manager.reconcileDeposits(ctx)
	require.ErrorContains(t, err, "unable to sync active deposits")
	require.Equal(t, Deposited, deposit.GetState())
	require.Zero(t, deposit.ConfirmationHeight)
	require.Empty(t, manager.activeDeposits)
	require.Equal(t, []fsm.StateType{Deposited}, updateStates)
	require.EqualValues(t, []int64{0}, updateHeights)
}

// TestReconcileDepositsDeactivatesBeforeActivationFailure verifies that a
// failed reactivation of one visible deposit does not leave another vanished
// deposit in the live active set.
func TestReconcileDepositsDeactivatesBeforeActivationFailure(t *testing.T) {
	ctx := context.Background()
	visibleOutpoint := wire.OutPoint{
		Hash:  chainhash.Hash{21},
		Index: 5,
	}
	vanishedOutpoint := wire.OutPoint{
		Hash:  chainhash.Hash{22},
		Index: 6,
	}

	visibleDeposit := &Deposit{
		OutPoint: visibleOutpoint,
		Value:    btcutil.Amount(100_000),
	}
	visibleDeposit.SetState(Deposited)

	vanishedDeposit := &Deposit{
		OutPoint: vanishedOutpoint,
		Value:    btcutil.Amount(100_000),
	}
	vanishedDeposit.SetState(Deposited)

	utxo := &lnwallet.Utxo{
		OutPoint:      visibleOutpoint,
		Value:         visibleDeposit.Value,
		Confirmations: 0,
	}

	mockAddressManager := new(mockAddressManager)
	mockAddressManager.On(
		"ListUnspent", mock.Anything, int32(0), int32(MaxConfs),
	).Return([]*lnwallet.Utxo{utxo}, nil)
	mockAddressManager.On(
		"GetStaticAddressParameters", mock.Anything,
	).Return((*script.Parameters)(nil), errors.New("fsm init failed"))

	manager := NewManager(&ManagerConfig{
		AddressManager: mockAddressManager,
		Store:          new(mockStore),
	})
	manager.deposits[visibleOutpoint] = visibleDeposit
	manager.deposits[vanishedOutpoint] = vanishedDeposit

	vanishedFsm := &FSM{
		deposit:  vanishedDeposit,
		stopChan: make(chan struct{}),
		quitChan: make(chan struct{}),
	}
	go func() {
		<-vanishedFsm.stopChan
		close(vanishedFsm.quitChan)
	}()
	manager.activeDeposits[vanishedOutpoint] = vanishedFsm

	_, err := manager.reconcileDeposits(ctx)
	require.ErrorContains(t, err, "unable to sync active deposits")
	require.Empty(t, manager.activeDeposits)

	select {
	case <-vanishedFsm.quitChan:

	case <-time.After(time.Second):
		t.Fatal("vanished deposit fsm did not stop")
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

	_, err = manager.reconcileDeposits(ctx)
	require.NoError(t, err)

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
