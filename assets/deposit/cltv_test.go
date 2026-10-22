package deposit

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/txscript"
	"github.com/lightninglabs/loop/assets/htlc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

// TestCLTVHtlcAddress binds both sides of a deposit-to-HTLC transition to the
// exact CLTV terms without changing the deposit's own relative refund delay.
func TestCLTVHtlcAddress(t *testing.T) {
	genesis := testAddressGenesis()
	kit := newAddressKit(t, &genesis, nil)
	client := &addressClient{genesis: genesis}
	hash := lntypes.Hash{1}
	const height uint32 = 1_000_000
	addr, contract, err := kit.NewCLTVHtlcAddr(
		t.Context(), client, 1000, hash, height,
	)
	require.NoError(t, err)
	require.Equal(t, htlc.AssetHTLCCLTVV1, contract.Policy())
	require.Equal(t, height, contract.RefundHeight())
	require.Zero(t, contract.CsvExpiry())
	require.Equal(t, uint32(144), kit.csvExpiry)
	require.True(t, kit.senderInternalPubKey.IsEqual(contract.SenderInternalPubKey()))
	require.True(t, kit.receiverInternalPubKey.IsEqual(contract.ReceiverInternalPubKey()))
	require.True(t, kit.senderScriptKey.IsEqual(contract.SenderScriptKey()))
	require.True(t, kit.receiverScriptKey.IsEqual(contract.ReceiverScriptKey()))
	decoded, err := kit.ValidateCLTVHtlcAddr(addr.Encoded, 1000, hash, height)
	require.NoError(t, err)
	key, err := decoded.TaprootOutputKey()
	require.NoError(t, err)
	require.Equal(t, addr.TaprootOutputKey, schnorr.SerializePubKey(key))
	_, err = kit.ValidateCLTVHtlcAddr(addr.Encoded, 1000, hash, height+1)
	require.Error(t, err)
	_, err = kit.ValidateCLTVHtlcAddr(addr.Encoded, 1000, lntypes.Hash{2}, height)
	require.Error(t, err)
	_, err = kit.ValidateCLTVHtlcAddr(addr.Encoded, 999, hash, height)
	require.Error(t, err)
	_, err = kit.ValidateHtlcAddr(addr.Encoded, 1000, hash, 144)
	require.Error(t, err)
	legacy, _, err := kit.NewHtlcAddr(t.Context(), client, 1000, hash, 144)
	require.NoError(t, err)
	_, err = kit.ValidateCLTVHtlcAddr(legacy.Encoded, 1000, hash, height)
	require.Error(t, err)

	// Invalid terms must fail locally without creating an address at tapd.
	before := client.calls
	for _, invalid := range []uint32{0, txscript.LockTimeThreshold} {
		_, _, err := kit.NewCLTVHtlcAddr(t.Context(), client, 1000, hash, invalid)
		require.Error(t, err)
	}
	require.Equal(t, before, client.calls)
}
