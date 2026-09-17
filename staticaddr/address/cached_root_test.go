package address

import (
	"context"
	"testing"

	"github.com/lightninglabs/loop/test"
	"github.com/stretchr/testify/require"
)

// TestCachedRootParameters verifies public lookups need no store read and
// cannot mutate the active root snapshot through their returned values.
func TestCachedRootParameters(t *testing.T) {
	t.Parallel()

	_, clientKey := test.CreateKey(1)
	_, serverKey := test.CreateKey(2)
	_, otherKey := test.CreateKey(3)
	root := &AddressParameters{
		ID:           1,
		Expiry:       144,
		PkScript:     []byte{0x51, 0x20, 0x01},
		ClientPubkey: clientKey,
		ServerPubkey: serverKey,
	}
	manager := &Manager{rootAddress: root}

	params, err := manager.GetLegacyParameters(t.Context())
	require.NoError(t, err)
	require.Equal(t, root, params)
	require.NotSame(t, root, params)

	params.Expiry++
	params.PkScript[0] = 0
	*params.ClientPubkey = *otherKey
	*params.ServerPubkey = *otherKey

	params, err = manager.GetStaticAddressParameters(t.Context())
	require.NoError(t, err)
	require.EqualValues(t, 144, params.Expiry)
	require.Equal(t, []byte{0x51, 0x20, 0x01}, params.PkScript)
	require.True(t, clientKey.IsEqual(params.ClientPubkey))
	require.True(t, serverKey.IsEqual(params.ServerPubkey))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = manager.GetLegacyParameters(ctx)
	require.ErrorIs(t, err, context.Canceled)

	manager.rootAddress = nil
	params, err = manager.GetLegacyParameters(t.Context())
	require.NoError(t, err)
	require.Nil(t, params)
	_, err = manager.GetStaticAddressParameters(t.Context())
	require.ErrorIs(t, err, ErrNoStaticAddress)
}
