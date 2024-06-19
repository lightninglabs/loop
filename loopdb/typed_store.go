package loopdb

import (
	"context"

	"github.com/lightninglabs/loop/loopdb/sqlc"
)

// BatchedQuerier implements all DB queries and ExecTx on *sqlc.Queries.
// It is implemented by BaseDB, SqliteSwapStore, etc.
type BatchedQuerier interface {
	sqlc.Querier

	// ExecTx is a wrapper for txBody to abstract the creation and commit of
	// a db transaction. The db transaction is embedded in a `*sqlc.Queries`
	// that txBody needs to use when executing each one of the queries that
	// need to be applied atomically.
	ExecTx(ctx context.Context, txOptions TxOptions,
		txBody func(*sqlc.Queries) error) error
}

// TypedStore is similar to BaseDB but provides parameterized ExecTx.
// It is used in other packages expecting ExecTx operating on subset of methods.
type TypedStore[Q any] struct {
	BatchedQuerier
}

// NewTypedStore wraps a db, replacing generic ExecTx method with the typed one.
func NewTypedStore[Q any](db BatchedQuerier) *TypedStore[Q] {
	// Make sure *sqlc.Queries can be casted to Q.
	_ = any((*sqlc.Queries)(nil)).(Q)

	return &TypedStore[Q]{
		BatchedQuerier: db,
	}
}

// ExecTx will execute the passed txBody, operating upon generic parameter Q
// (usually a storage interface) in a single transaction. The set of TxOptions
// are passed in to allow the caller to specify if a transaction is read-only.
func (s *TypedStore[Q]) ExecTx(ctx context.Context,
	txOptions TxOptions, txBody func(Q) error) error {

	return s.BatchedQuerier.ExecTx(ctx, txOptions,
		func(q *sqlc.Queries) error {
			return txBody(any(q).(Q))
		},
	)
}
