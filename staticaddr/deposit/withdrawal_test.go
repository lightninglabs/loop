package deposit

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/stretchr/testify/require"
)

type withdrawalSnapshotStore struct {
	Store

	mu      sync.Mutex
	rows    map[ID]*Deposit
	readErr error
	writes  int
}

func (s *withdrawalSnapshotStore) GetDeposit(ctx context.Context, id ID) (*Deposit, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.readErr != nil {
		return nil, s.readErr
	}
	return cloneWithdrawalDeposit(s.rows[id]), nil
}

func (s *withdrawalSnapshotStore) UpdateDeposit(_ context.Context, d *Deposit) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows[d.ID] = cloneWithdrawalDeposit(d)
	s.writes++
	return nil
}

func (s *withdrawalSnapshotStore) AllDeposits(context.Context) ([]*Deposit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var rows []*Deposit
	for _, row := range s.rows {
		rows = append(rows, cloneWithdrawalDeposit(row))
	}
	return rows, nil
}

func (s *withdrawalSnapshotStore) setState(id ID, state fsm.StateType) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows[id].state = state
}

func withdrawalTestManager(t *testing.T) (*Manager, *withdrawalSnapshotStore, *Deposit, *FSM) {
	t.Helper()
	context := newManagerTestContext(t)
	manager := context.manager
	deposit := &Deposit{
		ID:                 ID{1},
		Hash:               chainhash.Hash{1},
		state:              Deposited,
		Value:              100_000,
		ConfirmationHeight: 3,
	}
	store := &withdrawalSnapshotStore{
		rows: map[ID]*Deposit{deposit.ID: cloneWithdrawalDeposit(deposit)},
	}
	manager.cfg.Store = store
	machine, err := NewFSM(t.Context(), deposit, manager.cfg,
		manager.finalizedDepositChan, true)
	require.NoError(t, err)
	t.Cleanup(machine.Stop)
	manager.deposits[deposit.OutPoint] = deposit
	manager.activeDeposits[deposit.OutPoint] = machine
	return manager, store, deposit, machine
}

func TestWithdrawalReservationReseedsRuntime(t *testing.T) {
	manager, store, deposit, machine := withdrawalTestManager(t)
	outpoints := []wire.OutPoint{deposit.OutPoint}
	err := manager.WithWithdrawalLock(t.Context(), outpoints,
		func(selected []*Deposit) error {
			require.NotSame(t, deposit, selected[0])
			require.Equal(t, Deposited, selected[0].GetState())
			store.setState(deposit.ID, WithdrawalReserved)
			return nil
		})
	require.NoError(t, err)
	require.Equal(t, WithdrawalReserved, deposit.GetState())
	_, eligible := manager.AllOutpointsActiveDeposits(outpoints, Deposited)
	require.False(t, eligible)
	for _, event := range []fsm.EventType{OnLoopInInitiated, OnOpeningChannel, fsm.OnError} {
		err := manager.TransitionDeposits(t.Context(), []*Deposit{deposit},
			event, Deposited)
		require.Error(t, err)
	}
	// A queued expiry/recovery event executes against the reseeded machine,
	// never against its former Deposited state.
	require.NoError(t, machine.SendEvent(t.Context(), OnExpiry, nil))
	require.NoError(t, machine.SendEvent(t.Context(), OnRecover, nil))
	require.Equal(t, WithdrawalReserved, deposit.GetState())
	require.Zero(t, store.writes)

	store.setState(deposit.ID, Deposited)
	require.NoError(t, manager.ReconcileWithdrawal(t.Context(), outpoints))
	require.NoError(t, manager.TransitionDeposits(t.Context(), []*Deposit{deposit},
		OnOpeningChannel, OpeningChannel))
	require.Equal(t, OpeningChannel, deposit.GetState())
}

func TestWithdrawalCommitErrorStillReconciles(t *testing.T) {
	manager, store, deposit, _ := withdrawalTestManager(t)
	commitErr := errors.New("commit acknowledgement lost")
	err := manager.WithWithdrawalLock(t.Context(), []wire.OutPoint{deposit.OutPoint},
		func([]*Deposit) error {
			store.setState(deposit.ID, WithdrawalReserved)
			return commitErr
		})
	require.ErrorIs(t, err, commitErr)
	require.Equal(t, WithdrawalReserved, deposit.GetState())
	require.Zero(t, store.writes)
}

func TestWithdrawalReadbackFailureFencesAncillaryUpdates(t *testing.T) {
	manager, store, deposit, machine := withdrawalTestManager(t)
	outpoints := []wire.OutPoint{deposit.OutPoint}
	readErr := errors.New("database unavailable")
	err := manager.WithWithdrawalLock(t.Context(), outpoints,
		func([]*Deposit) error {
			store.setState(deposit.ID, WithdrawalReserved)
			store.mu.Lock()
			store.readErr = readErr
			store.mu.Unlock()
			return nil
		})
	require.ErrorIs(t, err, ErrWithdrawalReconciliationRequired)
	_, eligible := manager.AllOutpointsActiveDeposits(outpoints, fsm.EmptyState)
	require.False(t, eligible)
	available, err := manager.GetActiveDepositsInState(Deposited)
	require.NoError(t, err)
	require.Empty(t, available)
	require.ErrorIs(t, machine.SendEvent(t.Context(), OnExpiry, nil),
		ErrWithdrawalReconciliationRequired)

	// Confirmation updates used to write every cached field and a new state
	// history row. They must not overwrite the uncertain durable claim.
	require.NoError(t, manager.updateDepositConfirmations(t.Context(), []*lnwallet.Utxo{
		{OutPoint: deposit.OutPoint, Confirmations: 1},
	}, 10))
	require.Zero(t, store.writes)
	require.NoError(t, manager.syncActiveDeposits(t.Context(), nil))
	require.Same(t, machine, manager.activeDeposits[deposit.OutPoint])

	store.mu.Lock()
	store.readErr = nil
	store.mu.Unlock()
	require.NoError(t, manager.ReconcileWithdrawal(t.Context(), outpoints))
	require.Equal(t, WithdrawalReserved, deposit.GetState())
	require.False(t, machine.withdrawalFenced)
}

func TestWithdrawalLocksExcludeQueuedExpiryAndSelection(t *testing.T) {
	manager, store, deposit, machine := withdrawalTestManager(t)
	entered, release := make(chan struct{}), make(chan struct{})
	completed := make(chan error, 1)
	go func() {
		completed <- manager.WithWithdrawalLock(t.Context(),
			[]wire.OutPoint{deposit.OutPoint}, func([]*Deposit) error {
				close(entered)
				<-release
				store.setState(deposit.ID, WithdrawalReserved)
				return nil
			})
	}()
	<-entered
	expiry := make(chan error, 1)
	go func() { expiry <- machine.SendEvent(t.Context(), OnExpiry, nil) }()
	selected := make(chan bool, 1)
	go func() {
		_, ok := manager.AllOutpointsActiveDeposits(
			[]wire.OutPoint{deposit.OutPoint}, Deposited)
		selected <- ok
	}()
	select {
	case err := <-expiry:
		t.Fatalf("expiry raced active domain transaction: %v", err)
	case <-selected:
		t.Fatal("selection observed an uncommitted withdrawal")
	case <-time.After(10 * time.Millisecond):
	}
	close(release)
	require.NoError(t, <-completed)
	require.NoError(t, <-expiry)
	require.False(t, <-selected)
	require.Equal(t, WithdrawalReserved, deposit.GetState())
	require.Zero(t, store.writes)
}

func TestWithdrawalReconcileFinalizesWithoutWritingHistory(t *testing.T) {
	manager, store, deposit, machine := withdrawalTestManager(t)
	outpoints := []wire.OutPoint{deposit.OutPoint}
	store.setState(deposit.ID, Withdrawing)
	require.NoError(t, manager.ReconcileWithdrawal(t.Context(), outpoints))
	require.Error(t, manager.TransitionDeposits(t.Context(), []*Deposit{deposit},
		fsm.OnError, Deposited))
	store.setState(deposit.ID, Withdrawn)
	require.NoError(t, manager.ReconcileWithdrawal(t.Context(), outpoints))
	require.Equal(t, Withdrawn, deposit.GetState())
	require.NotContains(t, manager.activeDeposits, deposit.OutPoint)
	require.Zero(t, store.writes)
	select {
	case <-machine.quitChan:
	case <-time.After(time.Second):
		t.Fatal("finalized deposit worker did not stop")
	}
	// Repeated final-state reconciliation remains harmless.
	require.NoError(t, manager.ReconcileWithdrawal(t.Context(), outpoints))
}

func TestWithdrawalReservedStartupRecovery(t *testing.T) {
	original, store, deposit, _ := withdrawalTestManager(t)
	store.setState(deposit.ID, WithdrawalReserved)
	manager := NewManager(original.cfg)
	require.NoError(t, manager.recoverDeposits(t.Context()))
	t.Cleanup(func() {
		manager.mu.Lock()
		defer manager.mu.Unlock()
		for _, machine := range manager.activeDeposits {
			machine.Stop()
		}
	})
	selected, err := manager.GetActiveDepositsInState(WithdrawalReserved)
	require.NoError(t, err)
	require.Len(t, selected, 1)
	require.Equal(t, deposit.ID, selected[0].ID)
	require.NoError(t, manager.activeDeposits[deposit.OutPoint].SendEvent(
		t.Context(), OnRecover, nil))
	require.Zero(t, store.writes)
	available, err := manager.GetActiveDepositsInState(Deposited)
	require.NoError(t, err)
	require.Empty(t, available)
}

func TestWithdrawalPanicStillReconciles(t *testing.T) {
	manager, store, deposit, _ := withdrawalTestManager(t)
	require.Panics(t, func() {
		_ = manager.WithWithdrawalLock(t.Context(),
			[]wire.OutPoint{deposit.OutPoint}, func([]*Deposit) error {
				store.setState(deposit.ID, WithdrawalReserved)
				panic("after commit")
			})
	})
	require.Equal(t, WithdrawalReserved, deposit.GetState())
	_, eligible := manager.AllOutpointsActiveDeposits(
		[]wire.OutPoint{deposit.OutPoint}, Deposited)
	require.False(t, eligible)
	require.Zero(t, store.writes)
}
