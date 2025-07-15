package deposit

import (
	"context"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/taproot-assets/address"
	"github.com/lightningnetwork/lnd/clock"
)

// Querier is a subset of the methods we need from the postgres.querier
// interface for the deposit store.
type Querier interface {
	// This is intentionally left empty.
}

// DepositBaseDB is the interface that contains all the queries generated
// by sqlc for the deposit store. It also includes the ExecTx method for
// executing a function in the context of a database transaction.
type DepositBaseDB interface {
	Querier

	// ExecTx allows for executing a function in the context of a database
	// transaction.
	ExecTx(ctx context.Context, txOptions loopdb.TxOptions,
		txBody func(Querier) error) error
}

// SQLStore is the high level SQL store for deposits.
type SQLStore struct {
	db DepositBaseDB

	clock         clock.Clock
	addressParams address.ChainParams
}

// NewSQLStore creates a new SQLStore.
func NewSQLStore(db DepositBaseDB, clock clock.Clock,
	params *chaincfg.Params) *SQLStore {

	return &SQLStore{
		db:            db,
		clock:         clock,
		addressParams: address.ParamsForChain(params.Name),
	}
}

// UpdateDeposit updates the deposit state and extends the depsoit update log
// the SQL store.
func (s *SQLStore) UpdateDeposit(ctx context.Context, d *Deposit) error {
	return nil
}
