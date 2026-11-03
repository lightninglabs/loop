package withdraw

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	fsmv2 "github.com/lightninglabs/loop/fsm/v2"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/loopdb/sqlc"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/stretchr/testify/require"
)

type persistenceFixture struct {
	db       *loopdb.BaseDB
	store    *SqlStore
	deposits *deposit.SqlStore
	inputs   []*deposit.Deposit
	key      *btcec.PrivateKey
	script   []byte
}

func newPersistenceFixture(t *testing.T, count int) *persistenceFixture {
	t.Helper()
	db := loopdb.NewTestDB(t)
	return newPersistenceFixtureDB(t, db.BaseDB, count)
}

func newPersistenceFixtureDB(t *testing.T, db *loopdb.BaseDB, count int) *persistenceFixture {
	t.Helper()
	ds := deposit.NewSqlStore(db)
	store := NewSqlStore(loopdb.NewTypedStore[Querier](db), ds)
	key, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{3}, 32))
	script, err := txscript.PayToTaprootScript(txscript.ComputeTaprootKeyNoScript(key.PubKey()))
	require.NoError(t, err)
	f := &persistenceFixture{db: db, store: store, deposits: ds, key: key, script: script}
	require.NoError(t, db.CreateStaticAddress(t.Context(), sqlc.CreateStaticAddressParams{
		ClientPubkey: key.PubKey().SerializeCompressed(), ServerPubkey: key.PubKey().SerializeCompressed(),
		Pkscript: script, Expiry: 100, InitiationHeight: 100,
	}))
	for i := range count {
		d := &deposit.Deposit{ID: deposit.ID{byte(i + 1)}, Hash: chainhash.Hash{byte(i + 11)}, Index: uint32(i),
			Value: 100_000, ConfirmationHeight: 100, TimeOutSweepPkScript: []byte{0x00}}
		d.SetState(deposit.Deposited)
		require.NoError(t, ds.CreateDeposit(t.Context(), d))
		f.inputs = append(f.inputs, d)
	}
	return f
}

func (f *persistenceFixture) intent(t *testing.T, inputs []*deposit.Deposit, id byte) (WorkflowData, *wire.MsgTx) {
	t.Helper()
	address, err := btcutil.NewAddressWitnessPubKeyHash(bytes.Repeat([]byte{2}, 20), &chaincfg.RegressionNetParams)
	require.NoError(t, err)
	dest, err := txscript.PayToAddrScript(address)
	require.NoError(t, err)
	flowID := ID{id}
	data := WorkflowData{ID: hex.EncodeToString(flowID[:]), Attempt: 1, DestinationAddress: address.String(),
		DestinationScript: dest, FeeRateSatPerKWeight: 1000, InitiationHeight: 100,
		InitiationTime: time.Date(2026, time.October, 28, 10, 0, 0, 0, time.UTC)}
	tx := wire.NewMsgTx(2)
	prevouts := make(map[wire.OutPoint]*wire.TxOut)
	for _, d := range inputs {
		depID := hex.EncodeToString(d.ID[:])
		data.DepositIDs = append(data.DepositIDs, depID)
		data.Inputs = append(data.Inputs, WorkflowInput{
			DepositID: depID, Outpoint: d.OutPoint.String(), Value: int64(d.Value),
			PkScript: f.script, ConfirmationHeight: 100,
		})
		data.TotalDepositAmount += int64(d.Value)
		tx.AddTxIn(wire.NewTxIn(&d.OutPoint, nil, nil))
		tx.TxIn[len(tx.TxIn)-1].Sequence = wire.MaxTxInSequenceNum - 2
		prevouts[d.OutPoint] = &wire.TxOut{Value: int64(d.Value), PkScript: f.script}
	}
	slices.Sort(data.DepositIDs)
	tx.AddTxOut(&wire.TxOut{Value: data.TotalDepositAmount - 1000, PkScript: dest})
	data.UnsignedTx, err = encodeWorkflowTx(tx)
	require.NoError(t, err)
	fetcher := txscript.NewMultiPrevOutFetcher(prevouts)
	hashes := txscript.NewTxSigHashes(tx, fetcher)
	for i, in := range tx.TxIn {
		prev := prevouts[in.PreviousOutPoint]
		signature, err := txscript.RawTxInTaprootSignature(
			tx, hashes, i, prev.Value, prev.PkScript, nil, txscript.SigHashDefault, f.key,
		)
		require.NoError(t, err)
		in.Witness = wire.TxWitness{signature}
	}
	return data, tx
}

func persistenceRecord(t *testing.T, data WorkflowData, state fsmv2.StateID, revision uint64) fsmv2.Record {
	t.Helper()
	raw, err := fsmv2.JSONCodec[WorkflowData]().Encode(data)
	require.NoError(t, err)
	return fsmv2.Record{ID: data.ID, Version: 1, Revision: revision, State: state, Data: raw}
}

func TestWorkflowPersistence(t *testing.T) {
	f := newPersistenceFixture(t, 2)
	ctx := t.Context()
	store := f.store.WorkflowStore()
	data, tx := f.intent(t, f.inputs, 80)
	initial := persistenceRecord(t, data, WorkflowSigning, 1)
	require.NoError(t, store.Create(ctx, initial))
	loaded, err := store.Load(ctx, data.ID)
	require.NoError(t, err)
	require.Equal(t, initial, loaded)
	require.ErrorIs(t, store.Create(ctx, initial), fsmv2.ErrConflict)
	for _, d := range f.inputs {
		restored, err := f.deposits.GetDeposit(ctx, d.ID)
		require.NoError(t, err)
		require.EqualValues(t, workflowReservedDepositState, restored.GetState())
	}
	other := data
	otherID := ID{81}
	other.ID = hex.EncodeToString(otherID[:])
	require.ErrorIs(t, store.Create(ctx, persistenceRecord(t, other, WorkflowSigning, 1)), fsmv2.ErrConflict)
	_, err = store.Load(ctx, other.ID)
	require.ErrorIs(t, err, fsmv2.ErrNotFound)

	signed, err := encodeWorkflowTx(tx)
	require.NoError(t, err)
	data.Attempts = []WorkflowAttempt{{ID: 1, Tx: signed, TxID: tx.TxHash().String()}}
	pending := persistenceRecord(t, data, WorkflowPublishPending, 2)
	require.NoError(t, store.CompareAndSwap(ctx, pending, 1))
	require.ErrorIs(t, store.CompareAndSwap(ctx, pending, 1), fsmv2.ErrConflict)
	for _, d := range f.inputs {
		restored, err := f.deposits.GetDeposit(ctx, d.ID)
		require.NoError(t, err)
		require.Equal(t, deposit.Withdrawing, restored.GetState())
		require.Equal(t, tx.TxHash(), restored.FinalizedWithdrawalTx.TxHash())
	}
	exact, err := store.FindByDeposits(ctx, []deposit.ID{f.inputs[1].ID, f.inputs[0].ID})
	require.NoError(t, err)
	require.Equal(t, pending, exact)
	_, err = store.FindByDeposits(ctx, []deposit.ID{f.inputs[0].ID})
	require.ErrorIs(t, err, fsmv2.ErrConflict)
	data.WinningTxID = tx.TxHash().String()
	data.ConfirmationHeight = 120
	data.ConfirmationBlockHash = chainhash.Hash{9}.String()
	data.WithdrawnAmount = tx.TxOut[0].Value
	require.NoError(t, store.CompareAndSwap(ctx, persistenceRecord(t, data, WorkflowFinalizing, 3), 2))
	require.NoError(t, store.CompareAndSwap(ctx, persistenceRecord(t, data, WorkflowSucceeded, 4), 3))
	active, err := store.ListActive(ctx)
	require.NoError(t, err)
	require.Empty(t, active)
	for _, d := range f.inputs {
		restored, err := f.deposits.GetDeposit(ctx, d.ID)
		require.NoError(t, err)
		require.Equal(t, deposit.Withdrawn, restored.GetState())
		_, err = f.db.GetWithdrawalWorkflowClaim(ctx, d.ID[:])
		require.ErrorIs(t, err, sql.ErrNoRows)
	}
	history, err := f.db.GetWithdrawalWorkflowUpdates(ctx, data.ID)
	require.NoError(t, err)
	require.Len(t, history, 4)
	listings, err := f.store.GetAllWithdrawals(ctx)
	require.NoError(t, err)
	require.Len(t, listings, 1)
	require.EqualValues(t, data.TotalDepositAmount, listings[0].TotalDepositAmount)
	require.Equal(t, tx.TxHash(), listings[0].TxID)
}

type persistenceFailQueries struct {
	Querier

	err error
}

func (q persistenceFailQueries) InsertWithdrawalWorkflowUpdate(context.Context,
	sqlc.InsertWithdrawalWorkflowUpdateParams) error {

	return q.err
}

type persistenceFailDB struct {
	BaseDB

	err error
}

func (db persistenceFailDB) ExecTx(ctx context.Context, opts loopdb.TxOptions, body func(Querier) error) error {
	return db.BaseDB.ExecTx(ctx, opts, func(q Querier) error {
		return body(persistenceFailQueries{Querier: q, err: db.err})
	})
}

func TestWorkflowTransactionRollback(t *testing.T) {
	f := newPersistenceFixture(t, 1)
	ctx := t.Context()
	data, tx := f.intent(t, f.inputs, 90)
	injected := errors.New("history append failure")
	broken := NewSqlStore(persistenceFailDB{BaseDB: f.store.baseDB, err: injected}, f.deposits).WorkflowStore()
	initial := persistenceRecord(t, data, WorkflowSigning, 1)
	require.ErrorIs(t, broken.Create(ctx, initial), injected)
	_, err := f.store.WorkflowStore().Load(ctx, data.ID)
	require.ErrorIs(t, err, fsmv2.ErrNotFound)
	restored, err := f.deposits.GetDeposit(ctx, f.inputs[0].ID)
	require.NoError(t, err)
	require.Equal(t, deposit.Deposited, restored.GetState())
	require.NoError(t, f.store.WorkflowStore().Create(ctx, initial))
	signed, err := encodeWorkflowTx(tx)
	require.NoError(t, err)
	data.Attempts = []WorkflowAttempt{{ID: 1, Tx: signed, TxID: tx.TxHash().String()}}
	require.ErrorIs(t, broken.CompareAndSwap(ctx, persistenceRecord(t, data, WorkflowPublishPending, 2), 1), injected)
	loaded, err := f.store.WorkflowStore().Load(ctx, data.ID)
	require.NoError(t, err)
	require.Equal(t, initial, loaded)
	restored, err = f.deposits.GetDeposit(ctx, f.inputs[0].ID)
	require.NoError(t, err)
	require.EqualValues(t, workflowReservedDepositState, restored.GetState())
	require.Nil(t, restored.FinalizedWithdrawalTx)
}
