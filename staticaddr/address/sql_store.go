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

// CreateStaticAddress creates a static address record.
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
		Label:            addrParams.Label,
	}

	return s.baseDB.Queries.CreateStaticAddress(ctx, createArgs)
}

// GetStaticAddressID retrieves the database ID for a static address script.
func (s *SqlStore) GetStaticAddressID(ctx context.Context,
	pkScript []byte) (int32, error) {

	return s.baseDB.Queries.GetStaticAddressID(ctx, pkScript)
}

// ListStaticAddresses loads one bounded page without offset scans.
func (s *SqlStore) ListStaticAddresses(ctx context.Context, afterID,
	limit int32) ([]*AddressParameters, error) {

	if afterID < 0 || limit <= 0 {
		return nil, fmt.Errorf("invalid static address page: after=%d limit=%d",
			afterID, limit)
	}
	rows, err := s.baseDB.Queries.ListStaticAddresses(ctx,
		sqlc.ListStaticAddressesParams{AfterID: afterID, PageSize: limit})
	if err != nil {
		return nil, err
	}
	result := make([]*AddressParameters, 0, len(rows))
	for _, row := range rows {
		params, err := s.toAddressParameters(row)
		if err != nil {
			return nil, err
		}
		result = append(result, params)
	}
	return result, nil
}

// GetAllStaticAddresses returns all addresses known to the client.
func (s *SqlStore) GetAllStaticAddresses(ctx context.Context) (
	[]*AddressParameters, error) {

	staticAddresses, err := s.baseDB.Queries.AllStaticAddresses(ctx)
	if err != nil {
		return nil, err
	}

	result := make([]*AddressParameters, 0, len(staticAddresses))
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
func (s *SqlStore) GetLegacyParameters(ctx context.Context) (*AddressParameters,
	error) {

	staticAddress, err := s.baseDB.Queries.GetLegacyAddress(ctx)
	if err != nil {
		return nil, err
	}

	return s.toAddressParameters(staticAddress)
}

// UpdateStaticAddressLabel updates the local label for a static address by its
// pkScript, keeping relabeling a metadata-only database change that cannot
// create a new address record.
func (s *SqlStore) UpdateStaticAddressLabel(ctx context.Context,
	pkScript []byte, label string) error {

	updateArgs := sqlc.UpdateStaticAddressLabelParams{
		Pkscript: pkScript,
		Label:    label,
	}

	rowsAffected, err := s.baseDB.Queries.UpdateStaticAddressLabel(
		ctx, updateArgs,
	)
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		return fmt.Errorf("static address not found")
	}

	return nil
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
		Label:            row.Label,
	}, nil
}
