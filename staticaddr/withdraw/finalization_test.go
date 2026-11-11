package withdraw

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/loopdb/sqlc"
	"github.com/lightninglabs/loop/staticaddr/address"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/swap"
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
