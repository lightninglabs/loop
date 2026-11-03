package withdraw

import (
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	fsmv2 "github.com/lightninglabs/loop/fsm/v2"
	"github.com/lightninglabs/loop/loopdb/sqlc"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
)

func (f *persistenceFixture) saveLegacyDeposits(t *testing.T, inputs []*deposit.Deposit,
	tx *wire.MsgTx) {

	t.Helper()
	for _, d := range inputs {
		d.SetState(deposit.Withdrawing)
		d.Lock()
		d.FinalizedWithdrawalTx = tx
		err := f.deposits.UpdateDeposit(t.Context(), d)
		d.Unlock()
		require.NoError(t, err)
	}
}

// TestLegacyWithdrawalMigration imports summaries, valid pending clusters and
// unlinked in-flight deposits without changing any legacy withdrawal evidence.
func TestLegacyWithdrawalMigration(t *testing.T) {
	f := newPersistenceFixture(t, 7)
	ctx := t.Context()
	completed := f.inputs[:1]
	_, completedTx := f.intent(t, completed, 50)
	require.NoError(t, f.store.CreateWithdrawal(ctx, completed))
	require.NoError(t, f.store.UpdateWithdrawal(ctx, completed, completedTx, 150, f.script))
	completed[0].SetState(deposit.Withdrawn)
	completed[0].Lock()
	err := f.deposits.UpdateDeposit(ctx, completed[0])
	completed[0].Unlock()
	require.NoError(t, err)

	// Reverse signing order to ensure input order is reconstructed from the
	// durable transaction, independently of canonical membership ordering.
	pending := []*deposit.Deposit{f.inputs[2], f.inputs[1]}
	_, pendingTx := f.intent(t, pending, 51)
	require.NoError(t, f.store.CreateWithdrawal(ctx, pending))
	f.saveLegacyDeposits(t, pending, pendingTx)
	unknown := f.inputs[3:4]
	require.NoError(t, f.store.CreateWithdrawal(ctx, unknown))
	f.saveLegacyDeposits(t, unknown, nil)
	orphan := f.inputs[4:6]
	_, orphanTx := f.intent(t, orphan, 52)
	f.saveLegacyDeposits(t, orphan, orphanTx)
	f.saveLegacyDeposits(t, f.inputs[6:], nil)
	before, err := f.db.GetAllWithdrawals(ctx)
	require.NoError(t, err)
	require.Len(t, before, 3)
	beforeMembers := make(map[string][][]byte)
	for _, row := range before {
		ids, err := f.db.GetWithdrawalDeposits(ctx, row.WithdrawalID)
		require.NoError(t, err)
		beforeMembers[hex.EncodeToString(row.WithdrawalID)] = ids
	}
	listings, err := f.store.GetAllWithdrawals(ctx)
	require.NoError(t, err)
	require.Empty(t, listings)
	require.NoError(t, f.store.WorkflowStore().MigrateLegacy(ctx))
	rows, err := f.db.GetWithdrawalWorkflows(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 5)
	counts := make(map[fsmv2.StateID]int)
	for _, row := range rows {
		record, err := storeWorkflowRecord(row)
		require.NoError(t, err)
		counts[record.State]++
		require.NotEmpty(t, row.LegacySource.String)
		if record.State == WorkflowMonitoring {
			data, err := storeDecodeWorkflow(record)
			require.NoError(t, err)
			require.NoError(t, validateWorkflow(record.State, data))
			require.Len(t, data.Attempts, 1)
			if data.Attempts[0].TxID == pendingTx.TxHash().String() {
				require.Equal(t, pending[0].OutPoint.String(), data.Inputs[0].Outpoint)
			}
		}
	}
	require.Equal(t, 1, counts[WorkflowSucceeded])
	require.Equal(t, 2, counts[WorkflowMonitoring])
	require.Equal(t, 2, counts[WorkflowReconciliation])
	// The existing RPC can represent completed and signed pending transactions;
	// unsigned/quarantined imports with NULL txids stay out of that response.
	listings, err = f.store.GetAllWithdrawals(ctx)
	require.NoError(t, err)
	require.Len(t, listings, 3)
	var historical *Withdrawal
	for i := range listings {
		if listings[i].TxID == completedTx.TxHash() {
			historical = &listings[i]
		}
	}
	require.NotNil(t, historical)
	require.EqualValues(t, 150, historical.ConfirmationHeight)
	require.EqualValues(t, 100_000, historical.TotalDepositAmount)
	after, err := f.db.GetAllWithdrawals(ctx)
	require.NoError(t, err)
	require.Equal(t, before, after)
	for _, row := range after {
		ids, err := f.db.GetWithdrawalDeposits(ctx, row.WithdrawalID)
		require.NoError(t, err)
		require.Equal(t, beforeMembers[hex.EncodeToString(row.WithdrawalID)], ids)
	}
	require.NoError(t, f.store.WorkflowStore().MigrateLegacy(ctx))
	again, err := f.db.GetWithdrawalWorkflows(ctx)
	require.NoError(t, err)
	require.Equal(t, rows, again)
	for _, d := range []*deposit.Deposit{unknown[0], f.inputs[6]} {
		restored, err := f.deposits.GetDeposit(ctx, d.ID)
		require.NoError(t, err)
		require.EqualValues(t, workflowReservedDepositState, restored.GetState())
	}
}

func TestLegacyWithdrawalAmbiguity(t *testing.T) {
	f := newPersistenceFixture(t, 2)
	ctx := t.Context()
	_, tx := f.intent(t, f.inputs, 70)
	require.NoError(t, f.store.CreateWithdrawal(ctx, f.inputs))
	require.NoError(t, f.store.CreateWithdrawal(ctx, f.inputs))
	f.saveLegacyDeposits(t, f.inputs, tx)
	require.NoError(t, f.store.WorkflowStore().MigrateLegacy(ctx))
	records, err := f.store.WorkflowStore().ListActive(ctx)
	require.NoError(t, err)
	require.Len(t, records, 2)
	for _, record := range records {
		require.Equal(t, WorkflowReconciliation, record.State)
	}
	_, err = f.store.WorkflowStore().FindByDeposits(ctx, []deposit.ID{f.inputs[0].ID, f.inputs[1].ID})
	require.ErrorIs(t, err, fsmv2.ErrConflict)
	for _, d := range f.inputs {
		restored, err := f.deposits.GetDeposit(ctx, d.ID)
		require.NoError(t, err)
		require.EqualValues(t, workflowReservedDepositState, restored.GetState())
		_, err = f.db.GetWithdrawalWorkflowClaim(ctx, d.ID[:])
		require.ErrorIs(t, err, sql.ErrNoRows)
	}
	listings, err := f.store.GetAllWithdrawals(ctx)
	require.NoError(t, err)
	require.Empty(t, listings)
}

func TestWithdrawalClaimMonotonicTimestamp(t *testing.T) {
	f := newPersistenceFixture(t, 1)
	ctx := t.Context()
	d := f.inputs[0]
	future := time.Date(2030, time.January, 1, 0, 0, 0, 0, time.UTC)
	for _, state := range []string{string(deposit.Withdrawing), string(deposit.Deposited)} {
		require.NoError(t, f.db.InsertDepositUpdate(ctx, sqlc.InsertDepositUpdateParams{
			DepositID: d.ID[:], UpdateState: state, UpdateTimestamp: future,
		}))
	}
	f.store.clock = clock.NewTestClock(time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC))
	data, _ := f.intent(t, f.inputs, 93)
	require.NoError(t, f.store.WorkflowStore().Create(ctx, persistenceRecord(t, data, WorkflowSigning, 1)))
	restored, err := f.deposits.GetDeposit(ctx, d.ID)
	require.NoError(t, err)
	require.EqualValues(t, workflowReservedDepositState, restored.GetState())
	latest, err := f.db.GetLatestDepositUpdate(ctx, d.ID[:])
	require.NoError(t, err)
	require.True(t, latest.UpdateTimestamp.After(future))
}

func TestLegacyMigrationTransactionRollback(t *testing.T) {
	f := newPersistenceFixture(t, 1)
	ctx := t.Context()
	require.NoError(t, f.store.CreateWithdrawal(ctx, f.inputs))
	f.saveLegacyDeposits(t, f.inputs, nil)
	injected := context.Canceled
	broken := NewSqlStore(persistenceFailDB{BaseDB: f.store.baseDB, err: injected}, f.deposits).WorkflowStore()
	require.ErrorIs(t, broken.MigrateLegacy(ctx), injected)
	rows, err := f.db.GetWithdrawalWorkflows(ctx)
	require.NoError(t, err)
	require.Empty(t, rows)
	restored, err := f.deposits.GetDeposit(ctx, f.inputs[0].ID)
	require.NoError(t, err)
	require.Equal(t, deposit.Withdrawing, restored.GetState())
	require.NoError(t, f.store.WorkflowStore().MigrateLegacy(ctx))
	rows, err = f.db.GetWithdrawalWorkflows(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, string(WorkflowReconciliation), rows[0].State)
}

func TestLegacyMalformedAndPartialArtifacts(t *testing.T) {
	f := newPersistenceFixture(t, 2)
	ctx := t.Context()
	require.NoError(t, f.store.CreateWithdrawal(ctx, f.inputs[:1]))
	_, tx := f.intent(t, f.inputs, 94)
	f.saveLegacyDeposits(t, f.inputs, tx)
	require.NoError(t, f.store.WorkflowStore().MigrateLegacy(ctx))
	records, err := f.store.WorkflowStore().ListActive(ctx)
	require.NoError(t, err)
	require.Len(t, records, 2)
	for _, record := range records {
		require.Equal(t, WorkflowReconciliation, record.State)
	}
	// Neither half of the partial legacy cluster can become a new signing intent.
	data, _ := f.intent(t, f.inputs, 95)
	require.ErrorIs(t, f.store.WorkflowStore().Create(ctx,
		persistenceRecord(t, data, WorkflowSigning, 1)), fsmv2.ErrConflict)
}

func TestHistoricalSummaryWithoutArtifacts(t *testing.T) {
	f := newPersistenceFixture(t, 1)
	ctx := t.Context()
	require.NoError(t, f.store.CreateWithdrawal(ctx, f.inputs))
	rows, err := f.db.GetAllWithdrawals(ctx)
	require.NoError(t, err)
	txID := chainhash.Hash{5}.String()
	require.NoError(t, f.db.UpdateWithdrawal(ctx, sqlc.UpdateWithdrawalParams{
		WithdrawalID: rows[0].WithdrawalID, WithdrawalTxID: sql.NullString{String: txID, Valid: true},
		WithdrawnAmount:    sql.NullInt64{Int64: 99_000, Valid: true},
		ConfirmationHeight: sql.NullInt64{Int64: 500, Valid: true},
	}))
	require.NoError(t, f.store.WorkflowStore().MigrateLegacy(ctx))
	migrated, err := f.db.GetWithdrawalWorkflows(ctx)
	require.NoError(t, err)
	require.Len(t, migrated, 1)
	record, err := storeWorkflowRecord(migrated[0])
	require.NoError(t, err)
	require.Equal(t, WorkflowSucceeded, record.State)
	data, err := storeDecodeWorkflow(record)
	require.NoError(t, err)
	require.Empty(t, data.Attempts)
	listings, err := f.store.GetAllWithdrawals(ctx)
	require.NoError(t, err)
	require.Len(t, listings, 1)
	require.Equal(t, txID, listings[0].TxID.String())
	require.EqualValues(t, 99_000, listings[0].WithdrawnAmount)
}

// Corrupt legacy facts are preserved as explicit quarantine, not normalized
// into a plausible financial result or allowed to block all other imports.
func TestLegacyAccountingAnomalies(t *testing.T) {
	f := newPersistenceFixture(t, 1)
	ctx := t.Context()
	id := ID{111}
	require.NoError(t, f.db.CreateWithdrawal(ctx, sqlc.CreateWithdrawalParams{
		WithdrawalID: id[:], TotalDepositAmount: -1, InitiationTime: time.Time{},
	}))
	require.NoError(t, f.db.CreateWithdrawalDeposit(ctx, sqlc.CreateWithdrawalDepositParams{
		WithdrawalID: id[:], DepositID: f.inputs[0].ID[:],
	}))
	require.NoError(t, f.store.WorkflowStore().MigrateLegacy(ctx))
	records, err := f.store.WorkflowStore().ListActive(ctx)
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Equal(t, WorkflowReconciliation, records[0].State)
	data, err := storeDecodeWorkflow(records[0])
	require.NoError(t, err)
	require.EqualValues(t, -1, data.TotalDepositAmount)
	require.True(t, data.InitiationTime.IsZero())
	listings, err := f.store.GetAllWithdrawals(ctx)
	require.NoError(t, err)
	require.Empty(t, listings)
}

func TestLegacyCompletedDepositRepair(t *testing.T) {
	f := newPersistenceFixture(t, 1)
	ctx := t.Context()
	require.NoError(t, f.store.CreateWithdrawal(ctx, f.inputs))
	rows, err := f.db.GetAllWithdrawals(ctx)
	require.NoError(t, err)
	require.NoError(t, f.db.UpdateWithdrawal(ctx, sqlc.UpdateWithdrawalParams{
		WithdrawalID:       rows[0].WithdrawalID,
		WithdrawalTxID:     sql.NullString{String: chainhash.Hash{9}.String(), Valid: true},
		ConfirmationHeight: sql.NullInt64{Int64: 500, Valid: true},
		WithdrawnAmount:    sql.NullInt64{Int64: 99_000, Valid: true},
	}))
	require.NoError(t, f.store.WorkflowStore().MigrateLegacy(ctx))
	d := f.inputs[0]
	restored, err := f.deposits.GetDeposit(ctx, d.ID)
	require.NoError(t, err)
	require.Equal(t, deposit.Withdrawn, restored.GetState())
	// A previously imported terminal summary must still repair a stale mirror.
	future := time.Date(2035, time.January, 1, 0, 0, 0, 0, time.UTC)
	require.NoError(t, f.db.InsertDepositUpdate(ctx, sqlc.InsertDepositUpdateParams{
		DepositID: d.ID[:], UpdateState: string(deposit.Deposited), UpdateTimestamp: future,
	}))
	require.NoError(t, f.store.WorkflowStore().MigrateLegacy(ctx))
	restored, err = f.deposits.GetDeposit(ctx, d.ID)
	require.NoError(t, err)
	require.Equal(t, deposit.Withdrawn, restored.GetState())
	latest, err := f.db.GetLatestDepositUpdate(ctx, d.ID[:])
	require.NoError(t, err)
	require.True(t, latest.UpdateTimestamp.After(future))
	require.NoError(t, f.store.WorkflowStore().MigrateLegacy(ctx))
	again, err := f.db.GetLatestDepositUpdate(ctx, d.ID[:])
	require.NoError(t, err)
	require.Equal(t, latest, again)
}

func TestLegacyArtifactInDepositedState(t *testing.T) {
	f := newPersistenceFixture(t, 1)
	ctx := t.Context()
	_, tx := f.intent(t, f.inputs, 120)
	f.saveLegacyDeposits(t, f.inputs, tx)
	d := f.inputs[0]
	// The old FSM could return to Deposited after a failed action despite a
	// previously persisted signed transaction. That is evidence, not availability.
	d.SetState(deposit.Deposited)
	d.Lock()
	err := f.deposits.UpdateDeposit(ctx, d)
	d.Unlock()
	require.NoError(t, err)
	data, _ := f.intent(t, f.inputs, 121)
	require.ErrorIs(t, f.store.WorkflowStore().Create(ctx,
		persistenceRecord(t, data, WorkflowSigning, 1)), fsmv2.ErrConflict)
	require.NoError(t, f.store.WorkflowStore().MigrateLegacy(ctx))
	active, err := f.store.WorkflowStore().ListActive(ctx)
	require.NoError(t, err)
	require.Len(t, active, 1)
	require.Equal(t, WorkflowReconciliation, active[0].State)
	restored, err := f.deposits.GetDeposit(ctx, d.ID)
	require.NoError(t, err)
	require.EqualValues(t, workflowReservedDepositState, restored.GetState())
}

func TestLegacyPartialWithdrawalIntent(t *testing.T) {
	for _, owned := range []bool{true, false} {
		t.Run(fmt.Sprint("owned-change-", owned), func(t *testing.T) {
			f := newPersistenceFixture(t, 1)
			ctx := t.Context()
			_, tx := f.intent(t, f.inputs, 125)
			tx.TxOut[0].Value = 50_000
			changeScript := f.script
			if !owned {
				changeScript = tx.TxOut[0].PkScript
			}
			tx.AddTxOut(&wire.TxOut{Value: 49_000, PkScript: changeScript})
			prevouts := map[wire.OutPoint]*wire.TxOut{
				f.inputs[0].OutPoint: {Value: 100_000, PkScript: f.script},
			}
			tx.TxIn[0].Witness = nil
			hashes := txscript.NewTxSigHashes(tx, txscript.NewMultiPrevOutFetcher(prevouts))
			signature, err := txscript.RawTxInTaprootSignature(tx, hashes, 0, 100_000,
				f.script, nil, txscript.SigHashDefault, f.key)
			require.NoError(t, err)
			tx.TxIn[0].Witness = wire.TxWitness{signature}
			require.NoError(t, f.store.CreateWithdrawal(ctx, f.inputs))
			f.saveLegacyDeposits(t, f.inputs, tx)
			require.NoError(t, f.store.WorkflowStore().MigrateLegacy(ctx))
			records, err := f.store.WorkflowStore().ListActive(ctx)
			require.NoError(t, err)
			require.Len(t, records, 1)
			data, err := storeDecodeWorkflow(records[0])
			require.NoError(t, err)
			if owned {
				require.Equal(t, WorkflowMonitoring, records[0].State)
				require.EqualValues(t, 50_000, data.RequestedAmount)
				require.EqualValues(t, 49_000, data.ChangeAmount)
			} else {
				require.Equal(t, WorkflowReconciliation, records[0].State)
				require.Zero(t, data.RequestedAmount)
			}
		})
	}
}

func TestLegacyCompletedAccountingQuarantine(t *testing.T) {
	cases := []struct {
		name                     string
		memberCount              int
		total, withdrawn, change int64
	}{
		{name: "member-total-mismatch", memberCount: 2, total: 100_000, withdrawn: 99_000},
		{name: "output-sum-overflow", memberCount: 1, total: 100_000, withdrawn: math.MaxInt64, change: 1},
		{name: "outputs-exceed-total", memberCount: 1, total: 100_000, withdrawn: 90_000, change: 20_000},
		{name: "missing-members", memberCount: 0, total: 100_000, withdrawn: 99_000},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			f := newPersistenceFixture(t, test.memberCount)
			ctx := t.Context()
			id := ID{126}
			require.NoError(t, f.db.CreateWithdrawal(ctx, sqlc.CreateWithdrawalParams{
				WithdrawalID: id[:], TotalDepositAmount: test.total, InitiationTime: time.Now().UTC(),
			}))
			for _, d := range f.inputs {
				require.NoError(t, f.db.CreateWithdrawalDeposit(ctx, sqlc.CreateWithdrawalDepositParams{
					WithdrawalID: id[:], DepositID: d.ID[:],
				}))
			}
			require.NoError(t, f.db.UpdateWithdrawal(ctx, sqlc.UpdateWithdrawalParams{
				WithdrawalID: id[:], WithdrawalTxID: sql.NullString{String: chainhash.Hash{7}.String(), Valid: true},
				ConfirmationHeight: sql.NullInt64{Int64: 500, Valid: true},
				WithdrawnAmount:    sql.NullInt64{Int64: test.withdrawn, Valid: true},
				ChangeAmount:       sql.NullInt64{Int64: test.change, Valid: true},
			}))
			before, err := f.db.GetAllWithdrawals(ctx)
			require.NoError(t, err)
			require.NoError(t, f.store.WorkflowStore().MigrateLegacy(ctx))
			records, err := f.store.WorkflowStore().ListActive(ctx)
			require.NoError(t, err)
			require.Len(t, records, 1)
			require.Equal(t, WorkflowReconciliation, records[0].State)
			data, err := storeDecodeWorkflow(records[0])
			require.NoError(t, err)
			require.Equal(t, test.total, data.TotalDepositAmount)
			require.Equal(t, test.withdrawn, data.WithdrawnAmount)
			require.Equal(t, test.change, data.ChangeAmount)
			after, err := f.db.GetAllWithdrawals(ctx)
			require.NoError(t, err)
			require.Equal(t, before, after)
			for _, d := range f.inputs {
				restored, err := f.deposits.GetDeposit(ctx, d.ID)
				require.NoError(t, err)
				require.EqualValues(t, workflowReservedDepositState, restored.GetState())
			}
		})
	}
}

func TestLegacyPendingObservedTransactionListing(t *testing.T) {
	for _, txID := range []string{chainhash.Hash{8}.String(), "01"} {
		t.Run(txID, func(t *testing.T) {
			f := newPersistenceFixture(t, 1)
			ctx := t.Context()
			require.NoError(t, f.store.CreateWithdrawal(ctx, f.inputs))
			legacy, err := f.db.GetAllWithdrawals(ctx)
			require.NoError(t, err)
			require.NoError(t, f.db.UpdateWithdrawal(ctx, sqlc.UpdateWithdrawalParams{
				WithdrawalID: legacy[0].WithdrawalID, WithdrawalTxID: sql.NullString{String: txID, Valid: true},
				WithdrawnAmount: sql.NullInt64{Int64: 99_000, Valid: true},
			}))
			require.NoError(t, f.store.WorkflowStore().MigrateLegacy(ctx))
			active, err := f.store.WorkflowStore().ListActive(ctx)
			require.NoError(t, err)
			require.Len(t, active, 1)
			require.Equal(t, WorkflowReconciliation, active[0].State)
			data, err := storeDecodeWorkflow(active[0])
			require.NoError(t, err)
			require.Equal(t, txID, data.LegacyTxID)
			require.Empty(t, data.Attempts)
			require.Empty(t, data.WinningTxID)
			listing, err := f.store.GetAllWithdrawals(ctx)
			require.NoError(t, err)
			require.Len(t, listing, 1)
			expected, err := chainhash.NewHashFromStr(txID)
			require.NoError(t, err)
			require.Equal(t, *expected, listing[0].TxID)
			require.Zero(t, listing[0].ConfirmationHeight)
		})
	}
}

func TestLegacyMissingTransactionMirrors(t *testing.T) {
	for _, state := range []string{string(deposit.Withdrawing), string(deposit.Deposited)} {
		t.Run(state, func(t *testing.T) {
			f := newPersistenceFixture(t, 2)
			ctx := t.Context()
			require.NoError(t, f.store.CreateWithdrawal(ctx, f.inputs))
			_, tx := f.intent(t, f.inputs, 127)
			f.saveLegacyDeposits(t, f.inputs[:1], tx)
			if state == string(deposit.Withdrawing) {
				f.saveLegacyDeposits(t, f.inputs[1:], nil)
			}
			require.NoError(t, f.store.WorkflowStore().MigrateLegacy(ctx))
			active, err := f.store.WorkflowStore().ListActive(ctx)
			require.NoError(t, err)
			require.Len(t, active, 1)
			require.Equal(t, WorkflowMonitoring, active[0].State)
			for _, d := range f.inputs {
				restored, err := f.deposits.GetDeposit(ctx, d.ID)
				require.NoError(t, err)
				require.Equal(t, deposit.Withdrawing, restored.GetState())
				require.NotNil(t, restored.FinalizedWithdrawalTx)
				require.Equal(t, tx.TxHash(), restored.FinalizedWithdrawalTx.TxHash())
			}
		})
	}
	t.Run("orphan-without-membership", func(t *testing.T) {
		f := newPersistenceFixture(t, 2)
		ctx := t.Context()
		_, tx := f.intent(t, f.inputs, 128)
		f.saveLegacyDeposits(t, f.inputs[:1], tx)
		f.saveLegacyDeposits(t, f.inputs[1:], nil)
		require.NoError(t, f.store.WorkflowStore().MigrateLegacy(ctx))
		active, err := f.store.WorkflowStore().ListActive(ctx)
		require.NoError(t, err)
		require.Len(t, active, 2)
		for _, record := range active {
			require.Equal(t, WorkflowReconciliation, record.State)
		}
	})
	t.Run("conflicting-nonnull-artifacts", func(t *testing.T) {
		f := newPersistenceFixture(t, 2)
		ctx := t.Context()
		require.NoError(t, f.store.CreateWithdrawal(ctx, f.inputs))
		_, tx := f.intent(t, f.inputs, 129)
		f.saveLegacyDeposits(t, f.inputs[:1], tx)
		other := tx.Copy()
		other.TxOut[0].Value--
		f.saveLegacyDeposits(t, f.inputs[1:], other)
		require.NoError(t, f.store.WorkflowStore().MigrateLegacy(ctx))
		active, err := f.store.WorkflowStore().ListActive(ctx)
		require.NoError(t, err)
		require.Len(t, active, 1)
		require.Equal(t, WorkflowReconciliation, active[0].State)
	})
}

func TestLegacyCompletedConflictingOwner(t *testing.T) {
	for _, alreadyImported := range []bool{false, true} {
		t.Run(fmt.Sprint("previous-import-", alreadyImported), func(t *testing.T) {
			f := newPersistenceFixture(t, 1)
			ctx := t.Context()
			d := f.inputs[0]
			require.NoError(t, f.store.CreateWithdrawal(ctx, f.inputs))
			rows, err := f.db.GetAllWithdrawals(ctx)
			require.NoError(t, err)
			require.NoError(t, f.db.UpdateWithdrawal(ctx, sqlc.UpdateWithdrawalParams{
				WithdrawalID:       rows[0].WithdrawalID,
				WithdrawalTxID:     sql.NullString{String: chainhash.Hash{10}.String(), Valid: true},
				ConfirmationHeight: sql.NullInt64{Int64: 500, Valid: true},
				WithdrawnAmount:    sql.NullInt64{Int64: 99_000, Valid: true},
			}))
			if alreadyImported {
				require.NoError(t, f.store.WorkflowStore().MigrateLegacy(ctx))
			}
			d.SetState(deposit.LoopingIn)
			d.Lock()
			err = f.deposits.UpdateDeposit(ctx, d)
			d.Unlock()
			require.NoError(t, err)
			before, err := f.db.GetLatestDepositUpdate(ctx, d.ID[:])
			require.NoError(t, err)
			require.NoError(t, f.store.WorkflowStore().MigrateLegacy(ctx))
			active, err := f.store.WorkflowStore().ListActive(ctx)
			require.NoError(t, err)
			require.Len(t, active, 1)
			require.Equal(t, WorkflowReconciliation, active[0].State)
			restored, err := f.deposits.GetDeposit(ctx, d.ID)
			require.NoError(t, err)
			require.Equal(t, deposit.LoopingIn, restored.GetState())
			after, err := f.db.GetLatestDepositUpdate(ctx, d.ID[:])
			require.NoError(t, err)
			require.Equal(t, before, after)
			_, err = f.db.GetWithdrawalWorkflowClaim(ctx, d.ID[:])
			require.ErrorIs(t, err, sql.ErrNoRows)
			// Even if the other flow later releases the input, unresolved withdrawal
			// membership prevents a fresh signing operation without an exclusive claim.
			d.SetState(deposit.Deposited)
			d.Lock()
			err = f.deposits.UpdateDeposit(ctx, d)
			d.Unlock()
			require.NoError(t, err)
			data, _ := f.intent(t, f.inputs, 130)
			require.ErrorIs(t, f.store.WorkflowStore().Create(ctx,
				persistenceRecord(t, data, WorkflowSigning, 1)), fsmv2.ErrConflict)
		})
	}
}
