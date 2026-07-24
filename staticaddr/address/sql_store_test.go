package address

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/staticaddr/script"
	"github.com/lightninglabs/loop/staticaddr/version"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// TestSqlStoreStaticAddressLabel_whenCreatedAndUpdated proves labels survive
// the create and update path because operators rely on them after restarts.
func TestSqlStoreStaticAddressLabel_whenCreatedAndUpdated(t *testing.T) {
	ctx := t.Context()
	store := NewSqlStore(loopdb.NewTestDB(t).BaseDB)
	addrParams := testAddressParams(t)
	addrParams.Label = "first"

	err := store.CreateStaticAddress(ctx, addrParams)
	require.NoError(t, err)

	params, err := store.GetAllStaticAddresses(ctx)
	require.NoError(t, err)
	require.Len(t, params, 1)
	require.Equal(t, "first", params[0].Label)

	err = store.UpdateStaticAddressLabel(ctx, addrParams.PkScript, "second")
	require.NoError(t, err)

	params, err = store.GetAllStaticAddresses(ctx)
	require.NoError(t, err)
	require.Len(t, params, 1)
	require.Equal(t, "second", params[0].Label)
}

// TestSqlStoreStaticAddressLabel_whenCleared ensures an empty label is persisted
// as the supported way to remove local operator metadata.
func TestSqlStoreStaticAddressLabel_whenCleared(t *testing.T) {
	ctx := t.Context()
	store := NewSqlStore(loopdb.NewTestDB(t).BaseDB)
	addrParams := testAddressParams(t)
	addrParams.Label = "first"

	err := store.CreateStaticAddress(ctx, addrParams)
	require.NoError(t, err)

	err = store.UpdateStaticAddressLabel(ctx, addrParams.PkScript, "")
	require.NoError(t, err)

	params, err := store.GetAllStaticAddresses(ctx)
	require.NoError(t, err)
	require.Len(t, params, 1)
	require.Empty(t, params[0].Label)
}

// TestSqlStoreStaticAddressLabel_whenCreatedWithoutLabel protects unlabeled
// static addresses so label support remains optional metadata.
func TestSqlStoreStaticAddressLabel_whenCreatedWithoutLabel(t *testing.T) {
	ctx := t.Context()
	store := NewSqlStore(loopdb.NewTestDB(t).BaseDB)
	addrParams := testAddressParams(t)

	err := store.CreateStaticAddress(ctx, addrParams)
	require.NoError(t, err)

	params, err := store.GetAllStaticAddresses(ctx)
	require.NoError(t, err)
	require.Len(t, params, 1)
	require.Empty(t, params[0].Label)
}

// TestSqlStoreStaticAddressLabel_whenAddressMissing ensures relabel requests do
// not silently create metadata for an address the wallet does not own.
func TestSqlStoreStaticAddressLabel_whenAddressMissing(t *testing.T) {
	ctx := t.Context()
	store := NewSqlStore(loopdb.NewTestDB(t).BaseDB)
	addrParams := testAddressParams(t)

	err := store.UpdateStaticAddressLabel(ctx, addrParams.PkScript, "missing")
	require.Error(t, err)
	require.ErrorContains(t, err, "static address not found")
}

// testAddressParams builds a valid static-address record so label tests vary
// only metadata while preserving real script and key material constraints.
func testAddressParams(t *testing.T) *script.Parameters {
	t.Helper()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	serverKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	staticAddress, err := script.NewStaticAddress(
		input.MuSig2Version100RC2, int64(defaultExpiry), clientKey.PubKey(),
		serverKey.PubKey(),
	)
	require.NoError(t, err)

	pkScript, err := staticAddress.StaticAddressScript()
	require.NoError(t, err)

	return &script.Parameters{
		ClientPubkey: clientKey.PubKey(),
		ServerPubkey: serverKey.PubKey(),
		PkScript:     pkScript,
		Expiry:       defaultExpiry,
		KeyLocator: keychain.KeyLocator{
			Family: keychain.KeyFamily(1),
			Index:  2,
		},
		ProtocolVersion: version.AddressProtocolVersion(
			version.CurrentRPCProtocolVersion(),
		),
		InitiationHeight: 100,
	}
}
