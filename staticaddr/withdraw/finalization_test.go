package withdraw

import (
	"context"
	"database/sql"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/loopdb/sqlc"
	"github.com/lightninglabs/loop/staticaddr/address"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

func init() {
	UseLogger(btclog.Disabled)
}

// TestFinalizeWithdrawalRequiresHistory verifies that accounting is persisted
// before deposits become terminal and the pending withdrawal is removed.
func TestFinalizeWithdrawalRequiresHistory(t *testing.T) {
	t.Parallel()

	testErr := errors.New("persistence unavailable")
	for _, testCase := range []struct {
		name          string
		ambiguous     bool
		legacy        bool
		storeErr      error
		transitionErr error
		wantErr       string
	}{
		{
			name:      "ambiguous change remains pending",
			ambiguous: true,
			wantErr:   "multiple static-address change outputs",
		},
		{
			name:     "store failure remains pending",
			storeErr: testErr,
			wantErr:  "persisting withdrawal",
		},
		{
			name:          "transition failure remains pending",
			transitionErr: testErr,
			wantErr:       "transitioning withdrawn deposits",
		},
		{name: "confirmed derived change"},
		{name: "confirmed legacy root change", legacy: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			withdrawalScript := []byte{1}
			changeScript := []byte{2}
			changeFamily := swap.StaticAddressChangeKeyFamily
			if testCase.legacy {
				withdrawalScript = changeScript
				changeFamily = swap.StaticSingleAddressKeyFamily
			}
			addrManager := &withdrawalTestAddressManager{
				params: map[string]*address.AddressParameters{
					string(changeScript): {
						KeyLocator: keychain.KeyLocator{
							Family: keychain.KeyFamily(changeFamily),
						},
					},
					string([]byte{3}): {
						KeyLocator: keychain.KeyLocator{
							Family: keychain.KeyFamily(swap.StaticAddressChangeKeyFamily),
						},
					},
				},
			}
			tx := wire.NewMsgTx(2)
			tx.AddTxOut(&wire.TxOut{Value: 100_000, PkScript: withdrawalScript})
			tx.AddTxOut(&wire.TxOut{Value: 49_000, PkScript: changeScript})
			pendingTx := tx.Copy()
			originalTxHash := pendingTx.TxHash()
			if testCase.ambiguous {
				tx.AddTxOut(&wire.TxOut{Value: 1_000, PkScript: []byte{3}})
			}
			d := &deposit.Deposit{FinalizedWithdrawalTx: pendingTx}
			d.SetState(deposit.Withdrawing)

			updates, transitions := 0, 0
			persisted := false
			db := &withdrawalFinalizationDB{
				update: func(args sqlc.UpdateWithdrawalParams) error {
					updates++
					require.True(t, d.IsInState(deposit.Withdrawing))
					require.Equal(t, tx.TxHash().String(), args.WithdrawalTxID.String)
					require.EqualValues(t, 100_000, args.WithdrawnAmount.Int64)
					require.EqualValues(t, 49_000, args.ChangeAmount.Int64)
					require.EqualValues(t, 6, args.ConfirmationHeight.Int64)
					persisted = testCase.storeErr == nil

					return testCase.storeErr
				},
			}
			manager, err := NewManager(&ManagerConfig{
				AddressManager: addrManager,
				Store:          &SqlStore{baseDB: db},
				DepositManager: &withdrawalFinalizationDeposits{
					transition: func() error {
						transitions++
						require.True(t, persisted)
						if testCase.transitionErr == nil {
							d.SetState(deposit.Withdrawn)
						}

						return testCase.transitionErr
					},
				},
			}, 1)
			require.NoError(t, err)
			manager.finalizedWithdrawalTxns[originalTxHash] = pendingTx

			err = manager.finalizeWithdrawal(
				t.Context(), []*deposit.Deposit{d}, originalTxHash,
				withdrawalScript, tx, 6,
			)
			if testCase.wantErr != "" {
				require.ErrorContains(t, err, testCase.wantErr)
				require.True(t, d.IsInState(deposit.Withdrawing))
				require.Same(t, pendingTx,
					manager.finalizedWithdrawalTxns[originalTxHash])
			} else {
				require.NoError(t, err)
				require.True(t, d.IsInState(deposit.Withdrawn))
				require.Empty(t, manager.finalizedWithdrawalTxns)
			}
			if testCase.ambiguous {
				require.Zero(t, updates)
			} else {
				require.Equal(t, 1, updates)
			}
			if testCase.ambiguous || testCase.storeErr != nil {
				require.Zero(t, transitions)
			} else {
				require.Equal(t, 1, transitions)
			}

			if testCase.storeErr != nil {
				// Recovery can safely repeat accounting for a pending
				// withdrawal after a transient storage failure.
				testCase.storeErr = nil
				require.NoError(t, manager.finalizeWithdrawal(
					t.Context(), []*deposit.Deposit{d}, originalTxHash,
					withdrawalScript, tx, 6,
				))
				require.True(t, d.IsInState(deposit.Withdrawn))
				require.Empty(t, manager.finalizedWithdrawalTxns)
				require.Equal(t, 2, updates)
				require.Equal(t, 1, transitions)
			}
		})
	}
}

// TestFinalizeWithdrawalMissingHistory verifies that a failed initial history
// insert does not leave confirmed withdrawals pending indefinitely.
func TestFinalizeWithdrawalMissingHistory(t *testing.T) {
	t.Parallel()

	testDB := loopdb.NewTestDB(t)
	t.Cleanup(func() { require.NoError(t, testDB.Close()) })
	store := NewSqlStore(loopdb.NewTypedStore[Querier](testDB), nil)
	withdrawalScript := []byte{0x51}
	tx := wire.NewMsgTx(2)
	tx.AddTxOut(wire.NewTxOut(99_000, withdrawalScript))
	txHash := tx.TxHash()
	d := &deposit.Deposit{ID: deposit.ID{1}, FinalizedWithdrawalTx: tx}
	d.SetState(deposit.Withdrawing)
	deposits := []*deposit.Deposit{d}

	// CreateWithdrawal failed earlier, so the real history lookup cannot
	// find a row. Confirmation must still retire the spent deposits.
	require.ErrorIs(t, store.UpdateWithdrawal(
		t.Context(), deposits, tx, 6, nil,
	), sql.ErrNoRows)
	transitions := 0
	manager, err := NewManager(&ManagerConfig{
		Store: store,
		DepositManager: &withdrawalFinalizationDeposits{
			transition: func() error {
				transitions++
				require.True(t, d.IsInState(deposit.Withdrawing))
				d.SetState(deposit.Withdrawn)

				return nil
			},
		},
	}, 1)
	require.NoError(t, err)
	manager.finalizedWithdrawalTxns[txHash] = tx

	require.NoError(t, manager.finalizeWithdrawal(
		t.Context(), deposits, txHash, withdrawalScript, tx, 6,
	))
	require.Equal(t, 1, transitions)
	require.True(t, d.IsInState(deposit.Withdrawn))
	require.Empty(t, manager.finalizedWithdrawalTxns)
	history, err := store.GetAllWithdrawals(t.Context())
	require.NoError(t, err)
	require.Empty(t, history)
}

type withdrawalFinalizationDB struct {
	BaseDB

	update func(sqlc.UpdateWithdrawalParams) error
}

func (d *withdrawalFinalizationDB) ExecTx(_ context.Context, _ loopdb.TxOptions,
	body func(Querier) error) error {

	return body(d)
}

func (*withdrawalFinalizationDB) GetWithdrawalIDByDepositID(context.Context,
	[]byte) ([]byte, error) {

	return []byte{1}, nil
}

func (d *withdrawalFinalizationDB) UpdateWithdrawal(_ context.Context,
	args sqlc.UpdateWithdrawalParams) error {

	return d.update(args)
}

type withdrawalFinalizationDeposits struct {
	DepositManager

	transition func() error
}

func (d *withdrawalFinalizationDeposits) TransitionDeposits(context.Context,
	[]*deposit.Deposit, fsm.EventType, fsm.StateType) error {

	return d.transition()
}

// errBusyDB mimics the error the sqlite driver returns when another
// connection holds the write lock.
var errBusyDB = errors.New("database is locked (5) (SQLITE_BUSY)")

// finalizationNotifier reports the withdrawal as spent and confirmed as soon
// as the manager registers for the notifications.
type finalizationNotifier struct {
	lndclient.ChainNotifierClient

	tx *wire.MsgTx
}

func (n *finalizationNotifier) RegisterSpendNtfn(context.Context,
	*wire.OutPoint, []byte, int32, ...lndclient.NotifierOption) (
	chan *chainntnfs.SpendDetail, chan error, error) {

	txHash := n.tx.TxHash()
	spendChan := make(chan *chainntnfs.SpendDetail, 1)
	spendChan <- &chainntnfs.SpendDetail{
		SpenderTxHash:  &txHash,
		SpendingTx:     n.tx,
		SpendingHeight: 6,
	}

	return spendChan, make(chan error), nil
}

func (n *finalizationNotifier) RegisterConfirmationsNtfn(context.Context,
	*chainhash.Hash, []byte, int32, int32, ...lndclient.NotifierOption) (
	chan *chainntnfs.TxConfirmation, chan error, error) {

	confChan := make(chan *chainntnfs.TxConfirmation, 1)
	confChan <- &chainntnfs.TxConfirmation{Tx: n.tx, BlockHeight: 8}

	return confChan, make(chan error), nil
}

// newFinalizationRetryManager returns a manager whose confirmation handler
// sees the given withdrawal transaction confirm at once, together with the
// deposit that the transaction spends.
func newFinalizationRetryManager(t *testing.T, tx *wire.MsgTx,
	update func() error, transition func(*deposit.Deposit) error) (*Manager,
	*deposit.Deposit) {

	t.Helper()

	d := &deposit.Deposit{
		FinalizedWithdrawalTx: tx,
		AddressParams: &address.AddressParameters{
			PkScript: []byte{0x51},
		},
	}
	d.SetState(deposit.Withdrawing)

	manager, err := NewManager(&ManagerConfig{
		AddressManager: &withdrawalTestAddressManager{},
		ChainNotifier:  &finalizationNotifier{tx: tx},
		Store: &SqlStore{baseDB: &withdrawalFinalizationDB{
			update: func(sqlc.UpdateWithdrawalParams) error {
				return update()
			},
		}},
		DepositManager: &withdrawalFinalizationDeposits{
			transition: func() error {
				return transition(d)
			},
		},
	}, 1)
	require.NoError(t, err)

	manager.finalizeRetryDelay = time.Millisecond
	manager.finalizeRetryMaxDelay = 4 * time.Millisecond
	manager.finalizedWithdrawalTxns[tx.TxHash()] = tx

	return manager, d
}

// TestHandleWithdrawalRetriesFinalization verifies that the confirmation
// handler does not give up when finalizing a confirmed withdrawal fails for a
// transient reason. On signet two of three withdrawals that confirmed in the
// same block failed once with a busy database and stayed in the withdrawing
// state until the daemon was restarted.
func TestHandleWithdrawalRetriesFinalization(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name string

		// storeFailures and transitionFailures are the numbers of
		// attempts of each step that fail before the step succeeds.
		storeFailures      int32
		transitionFailures int32
	}{
		{name: "busy database once", storeFailures: 1},
		{name: "busy database repeatedly", storeFailures: 5},
		{name: "deposit transition fails once", transitionFailures: 1},
		{
			name:               "both steps fail",
			storeFailures:      2,
			transitionFailures: 2,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			tx := wire.NewMsgTx(2)
			tx.AddTxOut(&wire.TxOut{Value: 100_000, PkScript: []byte{1}})
			txHash := tx.TxHash()

			var updates, transitions atomic.Int32
			manager, d := newFinalizationRetryManager(
				t, tx,
				func() error {
					if updates.Add(1) <= testCase.storeFailures {
						return errBusyDB
					}

					return nil
				},
				func(d *deposit.Deposit) error {
					n := transitions.Add(1)
					if n <= testCase.transitionFailures {
						return errBusyDB
					}
					d.SetState(deposit.Withdrawn)

					return nil
				},
			)

			require.NoError(t, manager.handleWithdrawal(
				t.Context(), []*deposit.Deposit{d}, txHash, []byte{1},
			))

			require.Eventually(t, func() bool {
				manager.mu.Lock()
				defer manager.mu.Unlock()

				return d.IsInState(deposit.Withdrawn) &&
					len(manager.finalizedWithdrawalTxns) == 0
			}, 5*time.Second, time.Millisecond)

			// Every failed transition is preceded by a successful
			// history update, since the update runs first on each
			// attempt.
			require.Equal(
				t, testCase.transitionFailures+1, transitions.Load(),
			)
			require.Equal(
				t, testCase.storeFailures+
					testCase.transitionFailures+1, updates.Load(),
			)
		})
	}
}

// TestFinalizeWithdrawalRetryStopsOnCancel verifies that the retry loop ends
// when the context is canceled and leaves the withdrawal pending, so that
// recovery picks it up on the next start.
func TestFinalizeWithdrawalRetryStopsOnCancel(t *testing.T) {
	t.Parallel()

	tx := wire.NewMsgTx(2)
	tx.AddTxOut(&wire.TxOut{Value: 100_000, PkScript: []byte{1}})
	txHash := tx.TxHash()

	ctx, cancel := context.WithCancel(t.Context())
	var updates atomic.Int32
	manager, d := newFinalizationRetryManager(
		t, tx,
		func() error {
			// Cancel once the loop has demonstrably retried.
			if updates.Add(1) == 3 {
				cancel()
			}

			return errBusyDB
		},
		func(*deposit.Deposit) error {
			t.Error("deposits must not transition before the " +
				"history update succeeds")

			return nil
		},
	)

	err := manager.finalizeWithdrawalWithRetry(
		ctx, []*deposit.Deposit{d}, txHash, []byte{1}, tx, 6,
	)
	require.ErrorIs(t, err, errBusyDB)
	require.ErrorContains(t, err, "canceled after 3 attempts")
	require.EqualValues(t, 3, updates.Load())
	require.True(t, d.IsInState(deposit.Withdrawing))
	require.Same(t, tx, manager.finalizedWithdrawalTxns[txHash])
}

// TestNextFinalizeRetryDelay verifies that the delay between two finalization
// attempts doubles and never exceeds the maximum.
func TestNextFinalizeRetryDelay(t *testing.T) {
	t.Parallel()

	delay := defaultFinalizeRetryDelay
	var delays []time.Duration
	for range 8 {
		delays = append(delays, delay)
		delay = nextFinalizeRetryDelay(delay, defaultFinalizeRetryMaxDelay)
	}

	require.Equal(t, []time.Duration{
		time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second,
		16 * time.Second, 32 * time.Second, time.Minute, time.Minute,
	}, delays)

	// A maximum below the current delay lowers the delay to the maximum.
	require.Equal(
		t, time.Second, nextFinalizeRetryDelay(time.Minute, time.Second),
	)
}
