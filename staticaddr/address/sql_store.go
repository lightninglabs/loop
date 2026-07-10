package address

import (
	"context"

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
	addrParams *Parameters) error {

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

// GetAllStaticAddresses returns all addresses known to the client.
func (s *SqlStore) GetAllStaticAddresses(ctx context.Context) (
	[]*Parameters, error) {

	staticAddresses, err := s.baseDB.Queries.AllStaticAddresses(ctx)
	if err != nil {
		return nil, err
	}

	var result []*Parameters
	for _, address := range staticAddresses {
		res, err := s.toAddressParameters(address)
		if err != nil {
			return nil, err
		}

		result = append(result, res)
	}

	return result, nil
}

// GetLegacyParameters returns the first static address created for this L402.
func (s *SqlStore) GetLegacyParameters(ctx context.Context) (*Parameters,
	error) {

	staticAddress, err := s.baseDB.Queries.GetLegacyAddress(ctx)
	if err != nil {
		return nil, err
	}

	return s.toAddressParameters(staticAddress)
}

// toAddressParameters transforms a database representation of a static address
// to an AddressParameters struct.
func (s *SqlStore) toAddressParameters(row sqlc.StaticAddress) (
	*Parameters, error) {

	clientPubkey, err := btcec.ParsePubKey(row.ClientPubkey)
	if err != nil {
		return nil, err
	}

	serverPubkey, err := btcec.ParsePubKey(row.ServerPubkey)
	if err != nil {
		return nil, err
	}

	return &Parameters{
		ID:           row.ID,
		ClientPubkey: clientPubkey,
		ServerPubkey: serverPubkey,
		PkScript:     row.Pkscript,
		Expiry:       uint32(row.Expiry),
		KeyLocator: keychain.KeyLocator{
			Family: keychain.KeyFamily(row.ClientKeyFamily),
			Index:  uint32(row.ClientKeyIndex),
		},
		ProtocolVersion: version.AddressProtocolVersion(
			row.ProtocolVersion,
		),
		InitiationHeight: row.InitiationHeight,
	}, nil
}
