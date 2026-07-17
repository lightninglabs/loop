package deposit

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/loopdb/sqlc"
	"github.com/lightninglabs/loop/staticaddr/address"
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
	if deposit.AddressParams == nil {
		return fmt.Errorf("static address parameters must be set")
	}
	if deposit.AddressParams.ID <= 0 {
		return fmt.Errorf("static address ID must be set")
	}

	createArgs := sqlc.CreateDepositParams{
		DepositID:            deposit.ID[:],
		TxHash:               deposit.Hash[:],
		OutIndex:             int32(deposit.Index),
		Amount:               int64(deposit.Value),
		ConfirmationHeight:   deposit.GetConfirmationHeight(),
		TimeoutSweepPkScript: deposit.TimeOutSweepPkScript,
		StaticAddressID: sql.NullInt32{
			Int32: deposit.AddressParams.ID,
			Valid: true,
		},
	}

	// Store the on-chain cost if it was determined when the deposit was
	// found, otherwise leave it NULL so that it is determined later.
	cost := deposit.GetOnchainCost()
	if cost.Status != OnchainCostUndetermined {
		params, err := setOnchainCostParams(deposit.ID, cost)
		if err != nil {
			return err
		}

		createArgs.OnchainCost = params.OnchainCost
		createArgs.OnchainCostKnown = params.OnchainCostKnown
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
//
// Callers that pass a live deposit must hold the deposit lock while calling
// this method. The deposit FSM already does this for state transitions, and
// Manager.UpdateDeposit wraps external callers with the same lock.
func (s *SqlStore) UpdateDeposit(ctx context.Context, deposit *Deposit) error {
	insertUpdateArgs := sqlc.InsertDepositUpdateParams{
		DepositID:       deposit.ID[:],
		UpdateTimestamp: s.clock.Now().UTC(),
		UpdateState:     string(deposit.getStateNoLock()),
	}

	var (
		txHash   = deposit.Hash[:]
		outIndex = sql.NullInt32{
			Int32: int32(deposit.Index),
			Valid: true,
		}
		confirmationHeight = sql.NullInt64{
			Int64: deposit.GetConfirmationHeightNoLock(),
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

			deposit, err = ToDeposit(
				sqlc.AllDepositsRow(row), latestUpdate,
			)
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
				if errors.Is(err, sql.ErrNoRows) {
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

			deposit, err = ToDeposit(
				sqlc.AllDepositsRow(row), latestUpdate,
			)
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

// ToDeposit converts an sql deposit row with joined static address metadata to
// a deposit.
func ToDeposit(row sqlc.AllDepositsRow, lastUpdate sqlc.DepositUpdate) (*Deposit,
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

	onchainCost := toOnchainCost(row.OnchainCost, row.OnchainCostKnown)
	if !row.StaticAddressID.Valid || row.StaticAddressID.Int32 <= 0 {
		return nil, fmt.Errorf("deposit %x missing static address ID",
			row.DepositID)
	}

	deposit := &Deposit{
		ID:          id,
		OnchainCost: onchainCost,
		state:       fsm.StateType(lastUpdate.UpdateState),
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
	}

	deposit.AddressParams, err = address.ParametersFromRow(sqlc.StaticAddress{
		ID:               row.StaticAddressID.Int32,
		ClientPubkey:     row.ClientPubkey,
		ServerPubkey:     row.ServerPubkey,
		Expiry:           row.Expiry.Int32,
		Pkscript:         row.Pkscript,
		ClientKeyFamily:  row.ClientKeyFamily.Int32,
		ClientKeyIndex:   row.ClientKeyIndex.Int32,
		ProtocolVersion:  row.ProtocolVersion.Int32,
		InitiationHeight: row.InitiationHeight.Int32,
		Label:            row.Label.String,
	})
	if err != nil {
		return nil, err
	}

	return deposit, nil
}

// toOnchainCost converts the stored on-chain cost of a deposit.
func toOnchainCost(amount int64, known sql.NullBool) OnchainCost {
	switch {
	case !known.Valid:
		return OnchainCost{}

	case known.Bool:
		return OnchainCost{
			Amount: btcutil.Amount(amount),
			Status: OnchainCostKnown,
		}

	default:
		return OnchainCost{Status: OnchainCostUnknown}
	}
}

// SetOnchainCosts stores the on-chain cost of the given deposits. A deposit
// whose cost has already been determined keeps it.
func (s *SqlStore) SetOnchainCosts(ctx context.Context,
	costs map[ID]OnchainCost) error {

	if len(costs) == 0 {
		return nil
	}

	return s.baseDB.ExecTx(ctx, loopdb.NewSqlWriteOpts(),
		func(q *sqlc.Queries) error {
			for id, cost := range costs {
				params, err := setOnchainCostParams(id, cost)
				if err != nil {
					return err
				}

				err = q.SetDepositOnchainCost(ctx, params)
				if err != nil {
					return err
				}
			}

			return nil
		})
}

// setOnchainCostParams returns the parameters to store the given on-chain
// cost of a deposit. Only a determined cost can be stored.
func setOnchainCostParams(id ID,
	cost OnchainCost) (sqlc.SetDepositOnchainCostParams, error) {

	switch cost.Status {
	case OnchainCostKnown, OnchainCostUnknown:

	default:
		return sqlc.SetDepositOnchainCostParams{}, fmt.Errorf(
			"deposit %x: invalid on-chain cost status %d", id,
			cost.Status,
		)
	}

	return sqlc.SetDepositOnchainCostParams{
		DepositID:   id[:],
		OnchainCost: int64(cost.Amount),
		OnchainCostKnown: sql.NullBool{
			Bool:  cost.Status == OnchainCostKnown,
			Valid: true,
		},
	}, nil
}
