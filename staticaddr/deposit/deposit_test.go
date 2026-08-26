package deposit

import (
	"testing"

	"github.com/lightninglabs/loop/staticaddr/script"
	"github.com/lightninglabs/loop/staticaddr/version"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

func testAddressParameters(t *testing.T, id int32) *script.Parameters {
	t.Helper()

	_, clientKey := test.CreateKey(id)
	_, serverKey := test.CreateKey(id + 100)
	staticAddress, err := script.NewStaticAddress(
		input.MuSig2Version100RC2, int64(defaultExpiry), clientKey,
		serverKey,
	)
	require.NoError(t, err)

	pkScript, err := staticAddress.StaticAddressScript()
	require.NoError(t, err)

	return &script.Parameters{
		ID:           id,
		ClientPubkey: clientKey,
		ServerPubkey: serverKey,
		PkScript:     pkScript,
		Expiry:       defaultExpiry,
		KeyLocator: keychain.KeyLocator{
			Family: keychain.KeyFamily(swap.StaticAddressKeyFamily),
			Index:  uint32(id),
		},
		ProtocolVersion:  version.ProtocolVersion_V0,
		InitiationHeight: 100,
	}
}

// TestDepositIsExpiredUnconfirmed verifies that unconfirmed deposits do not
// expire because their CSV timeout has not started yet.
func TestDepositIsExpiredUnconfirmed(t *testing.T) {
	t.Parallel()

	d := &Deposit{}

	require.False(t, d.IsExpired(1_000, 144))
}

func TestGetStaticAddressScriptValidatesPersistedScript(t *testing.T) {
	t.Parallel()

	params := testAddressParameters(t, 1)
	d := &Deposit{AddressParams: params}

	_, err := d.GetStaticAddressScript()
	require.NoError(t, err)

	d.AddressParams.PkScript = []byte{0x51}
	_, err = d.GetStaticAddressScript()
	require.ErrorContains(t, err, "does not match persisted pkScript")

	d.AddressParams.ProtocolVersion = 999
	_, err = d.GetStaticAddressScript()
	require.ErrorContains(t, err, "unsupported static address protocol version")
}
