package address

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/staticaddr/script"
	"github.com/lightninglabs/loop/staticaddr/version"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// TestCreateStaticAddressSetsID verifies that address parameters are ready to
// own deposits immediately after they are persisted.
func TestCreateStaticAddressSetsID(t *testing.T) {
	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	serverKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	params := &script.Parameters{
		ClientPubkey: clientKey.PubKey(),
		ServerPubkey: serverKey.PubKey(),
		PkScript:     []byte{0x51},
		Expiry:       144,
		KeyLocator: keychain.KeyLocator{
			Family: 1,
			Index:  2,
		},
		ProtocolVersion:  version.ProtocolVersion_V0,
		InitiationHeight: 100,
	}

	db := loopdb.NewTestDB(t)
	store := NewSqlStore(db.BaseDB)
	require.NoError(t, store.CreateStaticAddress(t.Context(), params))
	require.Positive(t, params.ID)

	storedParams, err := store.GetAllStaticAddresses(t.Context())
	require.NoError(t, err)
	require.Len(t, storedParams, 1)
	require.Equal(t, params.ID, storedParams[0].ID)
}
