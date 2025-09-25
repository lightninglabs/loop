package deposit

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/jackc/pgx/v5"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/loopdb/sqlc"
	"github.com/lightninglabs/loop/staticaddr/address"
	"github.com/lightninglabs/loop/staticaddr/version"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/keychain"
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
	if deposit.AddressID <= 0 {
		return fmt.Errorf("static address ID must be set")
	}

	createArgs := sqlc.CreateDepositParams{
		DepositID:            deposit.ID[:],
		TxHash:               deposit.Hash[:],
		OutIndex:             int32(deposit.Index),
		Amount:               int64(deposit.Value),
		ConfirmationHeight:   deposit.ConfirmationHeight,
		TimeoutSweepPkScript: deposit.TimeOutSweepPkScript,
		StaticAddressID: sql.NullInt32{
			Int32: deposit.AddressID,
			Valid: true,
		},
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

			allDepositsRow := sqlc.AllDepositsRow{
				ID:                    row.ID,
				DepositID:             row.DepositID,
				TxHash:                row.TxHash,
				OutIndex:              row.OutIndex,
				Amount:                row.Amount,
				ConfirmationHeight:    row.ConfirmationHeight,
				TimeoutSweepPkScript:  row.TimeoutSweepPkScript,
				ExpirySweepTxid:       row.ExpirySweepTxid,
				FinalizedWithdrawalTx: row.FinalizedWithdrawalTx,
				SwapHash:              row.SwapHash,
				StaticAddressID:       row.StaticAddressID,
				ClientPubkey:          row.ClientPubkey,
				ServerPubkey:          row.ServerPubkey,
				Expiry:                row.Expiry,
			}
			deposit, err = ToDeposit(allDepositsRow, latestUpdate)
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

			allDepositsRow := sqlc.AllDepositsRow{
				ID:                    row.ID,
				DepositID:             row.DepositID,
				TxHash:                row.TxHash,
				OutIndex:              row.OutIndex,
				Amount:                row.Amount,
				ConfirmationHeight:    row.ConfirmationHeight,
				TimeoutSweepPkScript:  row.TimeoutSweepPkScript,
				ExpirySweepTxid:       row.ExpirySweepTxid,
				FinalizedWithdrawalTx: row.FinalizedWithdrawalTx,
				SwapHash:              row.SwapHash,
				ClientKeyFamily:       row.ClientKeyFamily,
				ClientKeyIndex:        row.ClientKeyIndex,
				StaticAddressID:       row.StaticAddressID,
				ClientPubkey:          row.ClientPubkey,
				ServerPubkey:          row.ServerPubkey,
				Expiry:                row.Expiry,
			}

			deposit, err = ToDeposit(allDepositsRow, latestUpdate)
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

			for _, d := range deposits {
				latestUpdate, err := q.GetLatestDepositUpdate(
					ctx, d.DepositID,
				)
				if err != nil {
					return err
				}

				d, err := ToDeposit(d, latestUpdate)
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
func ToDeposit(row sqlc.AllDepositsRow,
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

	clientPubkey, err := btcec.ParsePubKey(row.ClientPubkey)
	if err != nil {
		return nil, err
	}

	serverPubkey, err := btcec.ParsePubKey(row.ServerPubkey)
	if err != nil {
		return nil, err
	}

	params := &address.Parameters{
		ClientPubkey: clientPubkey,
		ServerPubkey: serverPubkey,
		Expiry:       uint32(row.Expiry.Int32),
		PkScript:     row.Pkscript,
		KeyLocator: keychain.KeyLocator{
			Family: keychain.KeyFamily(row.ClientKeyFamily.Int32),
			Index:  uint32(row.ClientKeyIndex.Int32),
		},
		ProtocolVersion: version.AddressProtocolVersion(
			row.ProtocolVersion.Int32,
		),
		InitiationHeight: row.InitiationHeight.Int32,
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
		AddressParams:         params,
		AddressID:             row.StaticAddressID.Int32,
	}, nil
}

// BatchSetStaticAddressID sets static_address_id for all deposits that are
// NULL.
func (s *SqlStore) BatchSetStaticAddressID(ctx context.Context,
	staticAddrID int32) error {

	if staticAddrID <= 0 {
		return fmt.Errorf("static address ID must be set")
	}

	return s.baseDB.ExecTx(
		ctx, loopdb.NewSqlWriteOpts(), func(q *sqlc.Queries) error {
			return q.SetAllNullDepositsStaticAddressID(
				ctx, sql.NullInt32{
					Int32: staticAddrID,
					Valid: true,
				},
			)
		},
	)
}
