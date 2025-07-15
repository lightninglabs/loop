package deposit

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/loopdb/sqlc"
	"github.com/lightninglabs/taproot-assets/address"
	"github.com/lightningnetwork/lnd/clock"
)

// Querier is a subset of the methods we need from the postgres.querier
// interface for the deposit store.
type Querier interface {
	AddAssetDeposit(context.Context, sqlc.AddAssetDepositParams) error

	UpdateDepositState(ctx context.Context,
		arg sqlc.UpdateDepositStateParams) error

	MarkDepositConfirmed(ctx context.Context,
		arg sqlc.MarkDepositConfirmedParams) error
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

// AddAssetDeposit adds a new asset deposit to the database.
func (s *SQLStore) AddAssetDeposit(ctx context.Context, d *Deposit) error {
	txOptions := loopdb.NewSqlWriteOpts()

	createdAt := d.CreatedAt.UTC()
	clientScriptPubKey := d.FunderScriptKey.SerializeCompressed()
	clientInternalPubKey := d.FunderInternalKey.SerializeCompressed()
	serverScriptPubKey := d.CoSignerScriptKey.SerializeCompressed()
	serverInternalPubKey := d.CoSignerInternalKey.SerializeCompressed()

	return s.db.ExecTx(ctx, txOptions, func(tx Querier) error {
		err := tx.AddAssetDeposit(ctx, sqlc.AddAssetDepositParams{
			DepositID:            d.ID,
			CreatedAt:            createdAt,
			AssetID:              d.AssetID[:],
			Amount:               int64(d.Amount),
			ClientScriptPubkey:   clientScriptPubKey,
			ClientInternalPubkey: clientInternalPubKey,
			ServerScriptPubkey:   serverScriptPubKey,
			ServerInternalPubkey: serverInternalPubKey,
			ClientKeyFamily:      int32(d.KeyLocator.Family),
			ClientKeyIndex:       int32(d.KeyLocator.Index),
			Expiry:               int32(d.CsvExpiry),
			Addr:                 d.Addr,
			ProtocolVersion:      int32(d.Version),
		})
		if err != nil {
			return err
		}

		return tx.UpdateDepositState(ctx, sqlc.UpdateDepositStateParams{
			DepositID:       d.ID,
			UpdateState:     int32(StateInitiated),
			UpdateTimestamp: createdAt,
		})
	})
}

// UpdateDeposit updates the deposit state and extends the depsoit update log
// the SQL store.
func (s *SQLStore) UpdateDeposit(ctx context.Context, d *Deposit) error {
	txOptions := loopdb.NewSqlWriteOpts()

	return s.db.ExecTx(ctx, txOptions, func(tx Querier) error {
		switch d.State {
		case StateConfirmed:
			err := tx.MarkDepositConfirmed(
				ctx, sqlc.MarkDepositConfirmedParams{
					DepositID: d.ID,
					ConfirmationHeight: sql.NullInt32{
						Int32: int32(
							d.ConfirmationHeight,
						),
						Valid: true,
					},
					Outpoint: sql.NullString{
						String: d.Outpoint.String(),
						Valid:  true,
					},
					PkScript: d.PkScript,
				},
			)
			if err != nil {
				return err
			}

		default:
			return fmt.Errorf("unimplemented deposit state "+
				"update: %v", d.State)
		}

		return tx.UpdateDepositState(
			ctx, sqlc.UpdateDepositStateParams{
				DepositID:       d.ID,
				UpdateState:     int32(d.State),
				UpdateTimestamp: s.clock.Now().UTC(),
			},
		)
	})
}
