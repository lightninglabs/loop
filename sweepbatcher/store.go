package sweepbatcher

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/loopdb/sqlc"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/lntypes"
)

type BaseDB interface {
	// ConfirmBatch confirms a batch by setting the state to confirmed.
	ConfirmBatch(ctx context.Context, id int32) error

	// GetBatchSweeps fetches all the sweeps that are part a batch.
	GetBatchSweeps(ctx context.Context, batchID int32) (
		[]sqlc.Sweep, error)

	// GetBatchSweptAmount returns the total amount of sats swept by a
	// (confirmed) batch.
	GetBatchSweptAmount(ctx context.Context, batchID int32) (int64, error)

	// GetSweepStatus returns true if the sweep has been completed.
	GetSweepStatus(ctx context.Context, swapHash []byte) (bool, error)

	// GetParentBatch fetches the parent batch of a completed sweep.
	GetParentBatch(ctx context.Context, swapHash []byte) (sqlc.SweepBatch,
		error)

	// GetUnconfirmedBatches fetches all the batches from the
	// database that are not in a confirmed state.
	GetUnconfirmedBatches(ctx context.Context) ([]sqlc.SweepBatch, error)

	// InsertBatch inserts a batch into the database, returning the id of
	// the inserted batch.
	InsertBatch(ctx context.Context, arg sqlc.InsertBatchParams) (
		int32, error)

	// DropBatch drops a batch from the database.
	DropBatch(ctx context.Context, id int32) error

	// UpdateBatch updates a batch in the database.
	UpdateBatch(ctx context.Context, arg sqlc.UpdateBatchParams) error

	// UpsertSweep inserts a sweep into the database, or updates an existing
	// sweep if it already exists.
	UpsertSweep(ctx context.Context, arg sqlc.UpsertSweepParams) error

	// ExecTx allows for executing a function in the context of a database
	// transaction.
	ExecTx(ctx context.Context, txOptions loopdb.TxOptions,
		txBody func(*sqlc.Queries) error) error
}

// SQLStore manages the reservations in the database.
type SQLStore struct {
	baseDb BaseDB

	network *chaincfg.Params
	clock   clock.Clock
}

// NewSQLStore creates a new SQLStore.
func NewSQLStore(db BaseDB, network *chaincfg.Params) *SQLStore {
	return &SQLStore{
		baseDb:  db,
		network: network,
		clock:   clock.NewDefaultClock(),
	}
}

// FetchUnconfirmedSweepBatches fetches all the batches from the database that
// are not in a confirmed state.
func (s *SQLStore) FetchUnconfirmedSweepBatches(ctx context.Context) (
	[]*dbBatch, error) {

	var batches []*dbBatch

	dbBatches, err := s.baseDb.GetUnconfirmedBatches(ctx)
	if err != nil {
		return nil, err
	}

	for _, dbBatch := range dbBatches {
		batch := convertBatchRow(dbBatch)
		if err != nil {
			return nil, err
		}

		batches = append(batches, batch)
	}

	return batches, err
}

// InsertSweepBatch inserts a batch into the database, returning the id of the
// inserted batch.
func (s *SQLStore) InsertSweepBatch(ctx context.Context, batch *dbBatch) (int32,
	error) {

	return s.baseDb.InsertBatch(ctx, batchToInsertArgs(*batch))
}

// DropBatch drops a batch from the database. Note that we only use this call
// for batches that have no sweeps and so we'd not be able to resume.
func (s *SQLStore) DropBatch(ctx context.Context, id int32) error {
	readOpts := loopdb.NewSqlWriteOpts()
	return s.baseDb.ExecTx(ctx, readOpts, func(tx *sqlc.Queries) error {
		dbSweeps, err := tx.GetBatchSweeps(ctx, id)
		if err != nil {
			return err
		}

		if len(dbSweeps) != 0 {
			return fmt.Errorf("cannot drop a non-empty batch")
		}

		return tx.DropBatch(ctx, id)
	})
}

// UpdateSweepBatch updates a batch in the database.
func (s *SQLStore) UpdateSweepBatch(ctx context.Context, batch *dbBatch) error {
	return s.baseDb.UpdateBatch(ctx, batchToUpdateArgs(*batch))
}

// ConfirmBatch confirms a batch by setting the state to confirmed.
func (s *SQLStore) ConfirmBatch(ctx context.Context, id int32) error {
	return s.baseDb.ConfirmBatch(ctx, id)
}

// FetchBatchSweeps fetches all the sweeps that are part a batch.
func (s *SQLStore) FetchBatchSweeps(ctx context.Context, id int32) (
	[]*dbSweep, error) {

	readOpts := loopdb.NewSqlReadOpts()
	var sweeps []*dbSweep

	err := s.baseDb.ExecTx(ctx, readOpts, func(tx *sqlc.Queries) error {
		dbSweeps, err := tx.GetBatchSweeps(ctx, id)
		if err != nil {
			return err
		}

		for _, dbSweep := range dbSweeps {
			sweep, err := s.convertSweepRow(dbSweep)
			if err != nil {
				return err
			}

			sweeps = append(sweeps, &sweep)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return sweeps, nil
}

// TotalSweptAmount returns the total amount swept by a (confirmed) batch.
func (s *SQLStore) TotalSweptAmount(ctx context.Context, id int32) (
	btcutil.Amount, error) {

	amt, err := s.baseDb.GetBatchSweptAmount(ctx, id)
	if err != nil {
		return 0, err
	}

	return btcutil.Amount(amt), nil
}

// GetParentBatch fetches the parent batch of a completed sweep.
func (s *SQLStore) GetParentBatch(ctx context.Context, swapHash lntypes.Hash) (
	*dbBatch, error) {

	batch, err := s.baseDb.GetParentBatch(ctx, swapHash[:])
	if err != nil {
		return nil, err
	}

	if err != nil {
		return nil, err
	}

	return convertBatchRow(batch), nil
}

// UpsertSweep inserts a sweep into the database, or updates an existing sweep
// if it already exists.
func (s *SQLStore) UpsertSweep(ctx context.Context, sweep *dbSweep) error {
	return s.baseDb.UpsertSweep(ctx, sweepToUpsertArgs(*sweep))
}

// GetSweepStatus returns true if the sweep has been completed.
func (s *SQLStore) GetSweepStatus(ctx context.Context, swapHash lntypes.Hash) (
	bool, error) {

	return s.baseDb.GetSweepStatus(ctx, swapHash[:])
}

type dbBatch struct {
	// ID is the unique identifier of the batch.
	ID int32

	// State is the current state of the batch.
	State string

	// BatchTxid is the txid of the batch transaction.
	BatchTxid chainhash.Hash

	// BatchPkScript is the pkscript of the batch transaction.
	BatchPkScript []byte

	// LastRbfHeight is the height at which the last RBF attempt was made.
	LastRbfHeight int32

	// LastRbfSatPerKw is the sat per kw of the last RBF attempt.
	LastRbfSatPerKw int32

	// MaxTimeoutDistance is the maximum timeout distance of the batch.
	MaxTimeoutDistance int32
}

type dbSweep struct {
	// ID is the unique identifier of the sweep.
	ID int32

	// BatchID is the ID of the batch that the sweep belongs to.
	BatchID int32

	// SwapHash is the hash of the swap that the sweep belongs to.
	SwapHash lntypes.Hash

	// Outpoint is the outpoint of the sweep.
	Outpoint wire.OutPoint

	// Amount is the amount of the sweep.
	Amount btcutil.Amount

	// Completed indicates whether this sweep is completed.
	Completed bool
}

// convertBatchRow converts a batch row from db to a sweepbatcher.Batch struct.
func convertBatchRow(row sqlc.SweepBatch) *dbBatch {
	batch := dbBatch{
		ID: row.ID,
	}

	if row.Confirmed {
		batch.State = batchOpen
	}

	if row.BatchTxID.Valid {
		err := chainhash.Decode(&batch.BatchTxid, row.BatchTxID.String)
		if err != nil {
			return nil
		}
	}

	batch.BatchPkScript = row.BatchPkScript

	if row.LastRbfHeight.Valid {
		batch.LastRbfHeight = row.LastRbfHeight.Int32
	}

	if row.LastRbfSatPerKw.Valid {
		batch.LastRbfSatPerKw = row.LastRbfSatPerKw.Int32
	}

	batch.MaxTimeoutDistance = row.MaxTimeoutDistance

	return &batch
}

// BatchToUpsertArgs converts a Batch struct to the arguments needed to insert
// it into the database.
func batchToInsertArgs(batch dbBatch) sqlc.InsertBatchParams {
	args := sqlc.InsertBatchParams{
		Confirmed: false,
		BatchTxID: sql.NullString{
			Valid:  true,
			String: batch.BatchTxid.String(),
		},
		BatchPkScript: batch.BatchPkScript,
		LastRbfHeight: sql.NullInt32{
			Valid: true,
			Int32: batch.LastRbfHeight,
		},
		LastRbfSatPerKw: sql.NullInt32{
			Valid: true,
			Int32: batch.LastRbfSatPerKw,
		},
		MaxTimeoutDistance: batch.MaxTimeoutDistance,
	}

	if batch.State == batchConfirmed {
		args.Confirmed = true
	}

	return args
}

// BatchToUpsertArgs converts a Batch struct to the arguments needed to insert
// it into the database.
func batchToUpdateArgs(batch dbBatch) sqlc.UpdateBatchParams {
	args := sqlc.UpdateBatchParams{
		ID:        batch.ID,
		Confirmed: false,
		BatchTxID: sql.NullString{
			Valid:  true,
			String: batch.BatchTxid.String(),
		},
		BatchPkScript: batch.BatchPkScript,
		LastRbfHeight: sql.NullInt32{
			Valid: true,
			Int32: batch.LastRbfHeight,
		},
		LastRbfSatPerKw: sql.NullInt32{
			Valid: true,
			Int32: batch.LastRbfSatPerKw,
		},
	}

	if batch.State == batchConfirmed {
		args.Confirmed = true
	}

	return args
}

// convertSweepRow converts a sweep row from db to a sweep struct.
func (s *SQLStore) convertSweepRow(row sqlc.Sweep) (dbSweep, error) {
	sweep := dbSweep{
		ID:      row.ID,
		BatchID: row.BatchID,
		Amount:  btcutil.Amount(row.Amt),
	}

	swapHash, err := lntypes.MakeHash(row.SwapHash)
	if err != nil {
		return sweep, err
	}

	sweep.SwapHash = swapHash

	hash, err := chainhash.NewHash(row.OutpointTxid)
	if err != nil {
		return sweep, err
	}

	sweep.Outpoint = wire.OutPoint{
		Hash:  *hash,
		Index: uint32(row.OutpointIndex),
	}

	return sweep, nil
}

// sweepToUpsertArgs converts a Sweep struct to the arguments needed to insert.
func sweepToUpsertArgs(sweep dbSweep) sqlc.UpsertSweepParams {
	return sqlc.UpsertSweepParams{
		SwapHash:      sweep.SwapHash[:],
		BatchID:       sweep.BatchID,
		OutpointTxid:  sweep.Outpoint.Hash[:],
		OutpointIndex: int32(sweep.Outpoint.Index),
		Amt:           int64(sweep.Amount),
		Completed:     sweep.Completed,
	}
}
