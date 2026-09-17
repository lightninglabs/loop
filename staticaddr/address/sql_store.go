package address

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/loopdb/sqlc"
	"github.com/lightninglabs/loop/staticaddr/version"
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

// CreateStaticAddress creates a static address record in the database.
func (s *SqlStore) CreateStaticAddress(ctx context.Context,
	addrParams *AddressParameters) error {

	createArgs := sqlc.CreateStaticAddressParams{
		ClientPubkey:     addrParams.ClientPubkey.SerializeCompressed(),
		ServerPubkey:     addrParams.ServerPubkey.SerializeCompressed(),
		Expiry:           int32(addrParams.Expiry),
		ClientKeyFamily:  int32(addrParams.KeyLocator.Family),
		ClientKeyIndex:   int32(addrParams.KeyLocator.Index),
		Pkscript:         addrParams.PkScript,
		ProtocolVersion:  int32(addrParams.ProtocolVersion),
		InitiationHeight: addrParams.InitiationHeight,
	}

	return s.baseDB.Queries.CreateStaticAddress(ctx, createArgs)
}

// GetStaticAddressID retrieves the database ID for a static address script.
func (s *SqlStore) GetStaticAddressID(ctx context.Context,
	pkScript []byte) (int32, error) {

	return s.baseDB.Queries.GetStaticAddressID(ctx, pkScript)
}

