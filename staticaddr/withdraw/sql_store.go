package withdraw

import (
	"bytes"
	"context"
	"strings"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/loopdb/sqlc"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightningnetwork/lnd/clock"
)

// SqlStore is the backing store for static address withdrawals.
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

// CreateWithdrawal creates a static address withdrawal record in the database.
func (s *SqlStore) CreateWithdrawal(ctx context.Context, tx *wire.MsgTx,
	confirmationHeight uint32, deposits []*deposit.Deposit,
	changePkScript []byte) error {

	strOutpoints := make([]string, len(deposits))
	totalAmount := int64(0)
	for i, deposit := range deposits {
		strOutpoints[i] = deposit.OutPoint.String()
		totalAmount += int64(deposit.Value)
	}

	// Populate the optional change amount.
	withdrawnAmount, changeAmount := int64(0), int64(0)
	if len(tx.TxOut) == 1 {
		withdrawnAmount = tx.TxOut[0].Value
	} else if len(tx.TxOut) == 2 {
		withdrawnAmount, changeAmount = tx.TxOut[0].Value, tx.TxOut[1].Value
		if bytes.Equal(changePkScript, tx.TxOut[0].PkScript) {
			changeAmount = tx.TxOut[0].Value
			withdrawnAmount = tx.TxOut[1].Value
		}
	}

	createArgs := sqlc.CreateWithdrawalParams{
		WithdrawalTxID:     tx.TxHash().String(),
		DepositOutpoints:   strings.Join(strOutpoints, ","),
		TotalDepositAmount: totalAmount,
		WithdrawnAmount:    withdrawnAmount,
		ChangeAmount:       changeAmount,
		ConfirmationHeight: int64(confirmationHeight),
	}

	return s.baseDB.ExecTx(ctx, &loopdb.SqliteTxOptions{},
		func(q *sqlc.Queries) error {
			return q.CreateWithdrawal(ctx, createArgs)
		})
}

// AllWithdrawals retrieves all known withdrawals.
func (s *SqlStore) AllWithdrawals(ctx context.Context) ([]*Withdrawal, error) {
	var allWithdrawals []*Withdrawal

	err := s.baseDB.ExecTx(ctx, loopdb.NewSqlReadOpts(),
		func(q *sqlc.Queries) error {
			var err error

			withdrawals, err := q.AllWithdrawals(ctx)
			if err != nil {
				return err
			}

			for _, withdrawal := range withdrawals {
				w, err := s.toWithdrawal(withdrawal)
				if err != nil {
					return err
				}

				allWithdrawals = append(allWithdrawals, w)
			}

			return nil
		})
	if err != nil {
		return nil, err
	}

	return allWithdrawals, nil
}

// toDeposit converts an sql deposit to a deposit.
func (s *SqlStore) toWithdrawal(row sqlc.Withdrawal) (*Withdrawal, error) {
	txHash, err := chainhash.NewHashFromStr(row.WithdrawalTxID)
	if err != nil {
		return nil, err
	}

	return &Withdrawal{
		TxID:               *txHash,
		DepositOutpoints:   strings.Split(row.DepositOutpoints, ","),
		TotalDepositAmount: btcutil.Amount(row.TotalDepositAmount),
		WithdrawnAmount:    btcutil.Amount(row.WithdrawnAmount),
		ChangeAmount:       btcutil.Amount(row.ChangeAmount),
		ConfirmationHeight: uint32(row.ConfirmationHeight),
	}, nil
}
