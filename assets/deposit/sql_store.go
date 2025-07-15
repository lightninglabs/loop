package deposit

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/loopdb/sqlc"
	"github.com/lightninglabs/taproot-assets/address"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/keychain"
)

// Querier is a subset of the methods we need from the postgres.querier
// interface for the deposit store.
type Querier interface {
	AddAssetDeposit(context.Context, sqlc.AddAssetDepositParams) error

	UpdateDepositState(ctx context.Context,
		arg sqlc.UpdateDepositStateParams) error

	MarkDepositConfirmed(ctx context.Context,
		arg sqlc.MarkDepositConfirmedParams) error

	GetAssetDeposits(ctx context.Context) ([]sqlc.GetAssetDepositsRow,
		error)
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

// GetAllDeposits returns all deposits known to the store.
func (s *SQLStore) GetAllDeposits(ctx context.Context) ([]Deposit, error) {
	sqlDeposits, err := s.db.GetAssetDeposits(ctx)
	if err != nil {
		return nil, err
	}

	deposits := make([]Deposit, 0, len(sqlDeposits))
	for _, sqlDeposit := range sqlDeposits {
		deposit, err := sqlcDepositToDeposit(
			sqlDeposit, &s.addressParams,
		)
		if err != nil {
			return nil, err
		}

		deposits = append(deposits, deposit)
	}

	return deposits, nil
}

func sqlcDepositToDeposit(sqlDeposit sqlc.GetAssetDepositsRow,
	addressParams *address.ChainParams) (Deposit, error) {

	clientScriptPubKey, err := btcec.ParsePubKey(
		sqlDeposit.ClientScriptPubkey,
	)
	if err != nil {
		return Deposit{}, err
	}

	serverScriptPubKey, err := btcec.ParsePubKey(
		sqlDeposit.ServerScriptPubkey,
	)
	if err != nil {
		return Deposit{}, err
	}

	clientInteralPubKey, err := btcec.ParsePubKey(
		sqlDeposit.ClientInternalPubkey,
	)
	if err != nil {
		return Deposit{}, err
	}

	serverInternalPubKey, err := btcec.ParsePubKey(
		sqlDeposit.ServerInternalPubkey,
	)
	if err != nil {
		return Deposit{}, err
	}

	clientKeyLocator := keychain.KeyLocator{
		Family: keychain.KeyFamily(
			sqlDeposit.ClientKeyFamily,
		),
		Index: uint32(sqlDeposit.ClientKeyIndex),
	}

	if len(sqlDeposit.AssetID) != len(asset.ID{}) {
		return Deposit{}, fmt.Errorf("malformed asset ID for deposit: "+
			"%v", sqlDeposit.DepositID)
	}

	depositInfo := &Info{
		ID: sqlDeposit.DepositID,
		Version: AssetDepositProtocolVersion(
			sqlDeposit.ProtocolVersion,
		),
		CreatedAt: sqlDeposit.CreatedAt.Local(), //nolint:gosmopolitan
		Amount:    uint64(sqlDeposit.Amount),
		Addr:      sqlDeposit.Addr,
		State:     State(sqlDeposit.UpdateState),
	}

	if sqlDeposit.ConfirmationHeight.Valid {
		depositInfo.ConfirmationHeight = uint32(
			sqlDeposit.ConfirmationHeight.Int32,
		)
	}

	if sqlDeposit.Outpoint.Valid {
		outpoint, err := wire.NewOutPointFromString(
			sqlDeposit.Outpoint.String,
		)
		if err != nil {
			return Deposit{}, err
		}

		depositInfo.Outpoint = outpoint
	}

	if len(sqlDeposit.SweepInternalPubkey) > 0 {
		sweepInternalPubKey, err := btcec.ParsePubKey(
			sqlDeposit.SweepInternalPubkey,
		)
		if err != nil {
			return Deposit{}, err
		}
		depositInfo.SweepInternalKey = sweepInternalPubKey
	}

	if len(sqlDeposit.SweepScriptPubkey) > 0 {
		sweepScriptPubKey, err := btcec.ParsePubKey(
			sqlDeposit.SweepScriptPubkey,
		)
		if err != nil {
			return Deposit{}, err
		}
		depositInfo.SweepScriptKey = sweepScriptPubKey
	}

	kit, err := NewKit(
		clientScriptPubKey, clientInteralPubKey, serverScriptPubKey,
		serverInternalPubKey, clientKeyLocator,
		asset.ID(sqlDeposit.AssetID), uint32(sqlDeposit.Expiry),
		addressParams,
	)
	if err != nil {
		return Deposit{}, err
	}

	return Deposit{
		Kit:  kit,
		Info: depositInfo,
	}, nil
}
