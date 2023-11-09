package staticaddr

import (
	"context"
	"errors"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/jackc/pgx/v4"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/loopdb/sqlc"
	"github.com/lightningnetwork/lnd/keychain"
)

// SqlStore is the backing store for static addresses.
type SqlStore struct {
	baseDB *loopdb.BaseDB
}

// NewSqlStore constructs a new SQLStore from a BaseDB. The BaseDB is agnostic
// to the underlying driver which can be postgres or sqlite.
func NewSqlStore(db *loopdb.BaseDB) *SqlStore {
	return &SqlStore{
		baseDB: db,
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

// CreateStaticAddress creates a static address record in the database.
func (s *SqlStore) CreateStaticAddress(ctx context.Context,
	addrParams *AddressParameters) error {

	createArgs := sqlc.CreateStaticAddressParams{
		ClientPubkey:    addrParams.ClientPubkey.SerializeCompressed(),
		ServerPubkey:    addrParams.ServerPubkey.SerializeCompressed(),
		Expiry:          int32(addrParams.Expiry),
		ClientKeyFamily: int32(addrParams.KeyLocator.Family),
		ClientKeyIndex:  int32(addrParams.KeyLocator.Index),
		Pkscript:        addrParams.PkScript,
		ProtocolVersion: int32(addrParams.ProtocolVersion),
	}

	return s.baseDB.Queries.CreateStaticAddress(ctx, createArgs)
}

// GetStaticAddress retrieves static address parameters for a given pkScript.
func (s *SqlStore) GetStaticAddress(ctx context.Context,
	pkScript []byte) (*AddressParameters, error) {

	staticAddress, err := s.baseDB.Queries.GetStaticAddress(ctx, pkScript)
	if err != nil {
		return nil, err
	}

	return s.toAddressParameters(staticAddress)
}

// GetAllStaticAddresses returns all address known to the server.
func (s *SqlStore) GetAllStaticAddresses(ctx context.Context) (
	[]*AddressParameters, error) {

	staticAddresses, err := s.baseDB.Queries.AllStaticAddresses(ctx)
	if err != nil {
		return nil, err
	}

	var result []*AddressParameters
	for _, address := range staticAddresses {
		res, err := s.toAddressParameters(address)
		if err != nil {
			return nil, err
		}

		result = append(result, res)
	}

	return result, nil
}

// Close closes the database connection.
func (s *SqlStore) Close() {
	s.baseDB.DB.Close()
}

// toAddressParameters transforms a database representation of a static address
// to an AddressParameters struct.
func (s *SqlStore) toAddressParameters(row sqlc.StaticAddress) (
	*AddressParameters, error) {

	clientPubkey, err := btcec.ParsePubKey(row.ClientPubkey)
	if err != nil {
		return nil, err
	}

	serverPubkey, err := btcec.ParsePubKey(row.ServerPubkey)
	if err != nil {
		return nil, err
	}

	return &AddressParameters{
		ClientPubkey: clientPubkey,
		ServerPubkey: serverPubkey,
		PkScript:     row.Pkscript,
		Expiry:       uint32(row.Expiry),
		KeyLocator: keychain.KeyLocator{
			Family: keychain.KeyFamily(row.ClientKeyFamily),
			Index:  uint32(row.ClientKeyIndex),
		},
		ProtocolVersion: AddressProtocolVersion(row.ProtocolVersion),
	}, nil
}
