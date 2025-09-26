package deposit

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"errors"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/jackc/pgx/v5"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/loopdb/sqlc"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/lntypes"
)

var (
	// ErrDepositNotFound is returned when a deposit is not found in the
	// database.
	ErrDepositNotFound = errors.New("deposit not found")
)

// SqlStore is the backing store for static address deposits.
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

// CreateDeposit creates a static address deposit record in the database.
func (s *SqlStore) CreateDeposit(ctx context.Context, deposit *Deposit) error {
	createArgs := sqlc.CreateDepositParams{
		DepositID:            deposit.ID[:],
		TxHash:               deposit.Hash[:],
		OutIndex:             int32(deposit.Index),
		Amount:               int64(deposit.Value),
		ConfirmationHeight:   deposit.ConfirmationHeight,
		TimeoutSweepPkScript: deposit.TimeOutSweepPkScript,
	}

	updateArgs := sqlc.InsertDepositUpdateParams{
		DepositID:       deposit.ID[:],
		UpdateTimestamp: s.clock.Now().UTC(),
		UpdateState:     string(deposit.GetState()),
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
		UpdateState:     string(deposit.state),
	}

	var (
		txHash   = deposit.Hash[:]
		outIndex = sql.NullInt32{
			Int32: int32(deposit.Index),
			Valid: true,
		}
		confirmationHeight = sql.NullInt64{
			Int64: deposit.ConfirmationHeight,
		}
	)

	var finalizedWithdrawalTx string
	if deposit.FinalizedWithdrawalTx != nil {
		var buffer bytes.Buffer
		err := deposit.FinalizedWithdrawalTx.Serialize(
			&buffer,
		)
		if err != nil {
			return err
		}

		finalizedWithdrawalTx = hex.EncodeToString(buffer.Bytes())
	}

	updateArgs := sqlc.UpdateDepositParams{
		DepositID:          deposit.ID[:],
		TxHash:             txHash,
		OutIndex:           outIndex.Int32,
		ConfirmationHeight: confirmationHeight.Int64,
		FinalizedWithdrawalTx: sql.NullString{
			String: finalizedWithdrawalTx,
			Valid:  finalizedWithdrawalTx != "",
		},
	}

	if deposit.ExpirySweepTxid != (chainhash.Hash{}) {
		updateArgs.ExpirySweepTxid = deposit.ExpirySweepTxid[:]
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
			row, err := q.GetDeposit(ctx, id[:])
			if err != nil {
				return err
			}

			latestUpdate, err := q.GetLatestDepositUpdate(
				ctx, id[:],
			)
			if err != nil {
				return err
			}

			deposit, err = ToDeposit(row, latestUpdate)
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

// DepositForOutpoint retrieves the deposit with the given outpoint from the
// database.
func (s *SqlStore) DepositForOutpoint(ctx context.Context,
	outpoint string) (*Deposit, error) {

	var deposit *Deposit
	err := s.baseDB.ExecTx(ctx, loopdb.NewSqlReadOpts(),
		func(q *sqlc.Queries) error {
			op, err := wire.NewOutPointFromString(outpoint)
			if err != nil {
				return err
			}
			params := sqlc.DepositForOutpointParams{
				TxHash:   op.Hash[:],
				OutIndex: int32(op.Index),
			}
			row, err := q.DepositForOutpoint(ctx, params)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) ||
					errors.Is(err, pgx.ErrNoRows) {

					return ErrDepositNotFound
				}

				return err
			}

			latestUpdate, err := q.GetLatestDepositUpdate(
				ctx, row.DepositID,
			)
			if err != nil {
				return err
			}

			deposit, err = ToDeposit(row, latestUpdate)
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
				latestUpdate, err := q.GetLatestDepositUpdate(
					ctx, deposit.DepositID,
				)
				if err != nil {
					return err
				}

				d, err := ToDeposit(deposit, latestUpdate)
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

// ToDeposit converts an sql deposit to a deposit.
func ToDeposit(row sqlc.Deposit, lastUpdate sqlc.DepositUpdate) (*Deposit,
	error) {

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

	var expirySweepTxid chainhash.Hash
	if row.ExpirySweepTxid != nil {
		hash, err := chainhash.NewHash(row.ExpirySweepTxid)
		if err != nil {
			return nil, err
		}
		expirySweepTxid = *hash
	}

	var finalizedWithdrawalTx *wire.MsgTx
	if row.FinalizedWithdrawalTx.Valid {
		finalizedWithdrawalTx = &wire.MsgTx{}
		tx, err := hex.DecodeString(row.FinalizedWithdrawalTx.String)
		if err != nil {
			return nil, err
		}

		err = finalizedWithdrawalTx.Deserialize(bytes.NewReader(tx))
		if err != nil {
			return nil, err
		}
	}

	var swapHash *lntypes.Hash
	if row.SwapHash != nil {
		hash, err := lntypes.MakeHash(row.SwapHash)
		if err != nil {
			return nil, err
		}

		swapHash = &hash
	}

	return &Deposit{
		ID:    id,
		state: fsm.StateType(lastUpdate.UpdateState),
		OutPoint: wire.OutPoint{
			Hash:  *txHash,
			Index: uint32(row.OutIndex),
		},
		Value:                 btcutil.Amount(row.Amount),
		ConfirmationHeight:    row.ConfirmationHeight,
		TimeOutSweepPkScript:  row.TimeoutSweepPkScript,
		ExpirySweepTxid:       expirySweepTxid,
		SwapHash:              swapHash,
		FinalizedWithdrawalTx: finalizedWithdrawalTx,
	}, nil
}
