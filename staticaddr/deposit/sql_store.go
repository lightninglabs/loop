package deposit

import (
	"context"
	"database/sql"
	"errors"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/jackc/pgx/v4"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/loopdb/sqlc"
	"github.com/lightningnetwork/lnd/clock"
)

// SqlStore is the backing store for static addresses.
type SqlStore struct {
	baseDB *loopdb.BaseDB

	clock clock.Clock
}

// NewSqlStore constructs a new SQLStore from a BaseDB. The BaseDB is agnostic
// to the underlying driver which can be postgres or sqlite.
func NewSqlStore(db *loopdb.BaseDB) *SqlStore {
	return &SqlStore{
		baseDB: db,

		clock: clock.NewDefaultClock(),
	}
}

// ExecTx is a wrapper for txBody to abstract the creation and commit of a db
// transaction. The db transaction is embedded in a `*sqlc.Queries` that txBody
// needs to use when executing each one of the queries that need to be applied
// atomically.
func (s *SqlStore) ExecTx(ctx context.Context, txOptions loopdb.TxOptions,
	txBody func(queries *sqlc.Queries) error) error {

	// Create the db transaction.
	tx, err := s.baseDB.BeginTx(ctx, txOptions)
	if err != nil {
		return err
	}

	// Rollback is safe to call even if the tx is already closed, so if the
	// tx commits successfully, this is a no-op.
	defer func() {
		err := tx.Rollback()
		switch {
		// If the tx was already closed (it was successfully executed)
		// we do not need to log that error.
		case errors.Is(err, pgx.ErrTxClosed):
			return

		// If this is an unexpected error, log it.
		case err != nil:
			log.Errorf("unable to rollback db tx: %v", err)
		}
	}()

	if err := txBody(s.baseDB.Queries.WithTx(tx)); err != nil {
		return err
	}

	// Commit transaction.
	return tx.Commit()
}

// CreateDeposit creates a static address record in the database.
func (s *SqlStore) CreateDeposit(ctx context.Context, deposit *Deposit) error {
	createArgs := sqlc.CreateDepositParams{
		DepositID:            deposit.ID[:],
		TxHash:               deposit.Hash[:],
		OutIndex:             int32(deposit.Index),
		Amount:               int64(deposit.Value),
		ConfirmationHeight:   deposit.ConfirmationHeight,
		TimeOutSweepPkScript: deposit.TimeOutSweepPkScript,
	}

	updateArgs := sqlc.InsertDepositUpdateParams{
		DepositID:       deposit.ID[:],
		UpdateTimestamp: s.clock.Now().UTC(),
		UpdateState:     string(deposit.State),
	}

	return s.baseDB.ExecTx(ctx, &loopdb.SqliteTxOptions{},
		func(q *sqlc.Queries) error {
			err := q.CreateDeposit(ctx, createArgs)
			if err != nil {
				return err
			}

			return q.InsertDepositUpdate(ctx, updateArgs)
		})
}

// UpdateDeposit updates the deposit in the database.
func (s *SqlStore) UpdateDeposit(ctx context.Context, deposit *Deposit) error {
	insertUpdateArgs := sqlc.InsertDepositUpdateParams{
		DepositID:       deposit.ID[:],
		UpdateTimestamp: s.clock.Now().UTC(),
		UpdateState:     string(deposit.State),
	}

	var (
		txHash   = deposit.Hash[:]
		outIndex = sql.NullInt32{
			Int32: int32(deposit.Index),
			Valid: true,
		}
	)

	updateArgs := sqlc.UpdateDepositParams{
		DepositID: deposit.ID[:],
		TxHash:    txHash,
		OutIndex:  outIndex.Int32,
		ConfirmationHeight: marshalSqlNullInt64(
			deposit.ConfirmationHeight,
		).Int64,
	}

	return s.baseDB.ExecTx(ctx, &loopdb.SqliteTxOptions{},
		func(q *sqlc.Queries) error {
			err := q.UpdateDeposit(ctx, updateArgs)
			if err != nil {
				return err
			}

			return q.InsertDepositUpdate(ctx, insertUpdateArgs)
		})
}

// GetDeposit retrieves the deposit from the database.
func (s *SqlStore) GetDeposit(ctx context.Context, id ID) (*Deposit, error) {
	var deposit *Deposit
	err := s.baseDB.ExecTx(ctx, loopdb.NewSqlReadOpts(),
		func(q *sqlc.Queries) error {
			var err error
			row, err := q.GetDeposit(ctx, id[:])
			if err != nil {
				return err
			}

			updates, err := q.GetDepositUpdates(ctx, id[:])
			if err != nil {
				return err
			}

			if len(updates) == 0 {
				return errors.New("no deposit updates")
			}

			deposit, err = s.toDeposit(row, updates[len(updates)-1])
			if err != nil {
				return err
			}

			return nil
		})
	if err != nil {
		return nil, err
	}

	return deposit, nil
}

// AllDeposits retrieves all known deposits to our static address.
func (s *SqlStore) AllDeposits(ctx context.Context) ([]*Deposit, error) {
	var allDeposits []*Deposit

	err := s.baseDB.ExecTx(ctx, loopdb.NewSqlReadOpts(),
		func(q *sqlc.Queries) error {
			var err error

			deposits, err := q.AllDeposits(ctx)
			if err != nil {
				return err
			}

			for _, deposit := range deposits {
				updates, err := q.GetDepositUpdates(
					ctx, deposit.DepositID,
				)
				if err != nil {
					return err
				}

				if len(updates) == 0 {
					return errors.New("no deposit updates")
				}

				lastUpdate := updates[len(updates)-1]

				d, err := s.toDeposit(deposit, lastUpdate)
				if err != nil {
					return err
				}

				allDeposits = append(allDeposits, d)
			}

			return nil
		})
	if err != nil {
		return nil, err
	}

	return allDeposits, nil
}

// toDeposit converts an sql deposit to a deposit.
func (s *SqlStore) toDeposit(row sqlc.Deposit,
	lastUpdate sqlc.DepositUpdate) (*Deposit, error) {

	id := ID{}
	err := id.FromByteSlice(row.DepositID)
	if err != nil {
		return nil, err
	}

	var txHash *chainhash.Hash
	if row.TxHash != nil {
		txHash, err = chainhash.NewHash(row.TxHash)
		if err != nil {
			return nil, err
		}
	}

	return &Deposit{
		ID:    id,
		State: fsm.StateType(lastUpdate.UpdateState),
		OutPoint: wire.OutPoint{
			Hash:  *txHash,
			Index: uint32(row.OutIndex),
		},
		Value:                btcutil.Amount(row.Amount),
		ConfirmationHeight:   row.ConfirmationHeight,
		TimeOutSweepPkScript: row.TimeOutSweepPkScript,
	}, nil
}

// Close closes the database connection.
func (s *SqlStore) Close() {
	s.baseDB.DB.Close()
}

// marshalSqlNullInt64 converts an int64 to a sql.NullInt64.
func marshalSqlNullInt64(i int64) sql.NullInt64 {
	return sql.NullInt64{
		Int64: i,
		Valid: i != 0,
	}
}
