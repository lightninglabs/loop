package loopin

import (
	"context"
	"testing"

	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/staticaddr/address"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/staticaddr/script"
	"github.com/lightninglabs/loop/staticaddr/version"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

func setPersistedTestDepositAddress(t *testing.T, ctx context.Context,
	db *loopdb.BaseDB, deposits ...*deposit.Deposit) {

	t.Helper()

	_, clientPubkey := test.CreateKey(101)
	_, serverPubkey := test.CreateKey(102)
	params := &script.Parameters{
		ClientPubkey: clientPubkey,
		ServerPubkey: serverPubkey,
		Expiry:       144,
		KeyLocator: keychain.KeyLocator{
			Family: 123,
			Index:  456,
		},
		PkScript:         []byte{0x51, 0x20, 0x01},
		ProtocolVersion:  version.ProtocolVersion_V0,
		InitiationHeight: 789,
	}

	addressStore := address.NewSqlStore(db)
	require.NoError(t, addressStore.CreateStaticAddress(ctx, params))

	var err error
	params.ID, err = addressStore.GetStaticAddressID(ctx, params.PkScript)
	require.NoError(t, err)

	for _, d := range deposits {
		d.AddressParams = params
	}
}
