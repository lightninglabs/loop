package reservation

import (
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/loop/swapkit"
	"github.com/lightninglabs/taproot-assets/address"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// TestReservationContractVector fixes the server-funded CSV contract, asset
// script key, and Bitcoin output that both sides must agree on.
func TestReservationContractVector(t *testing.T) {
	serverScalar := make([]byte, 32)
	clientScalar := make([]byte, 32)
	serverScalar[31] = 1
	clientScalar[31] = 2
	_, server := btcec.PrivKeyFromBytes(serverScalar)
	_, client := btcec.PrivKeyFromBytes(clientScalar)
	kit, err := NewKit(server, client, keychain.KeyLocator{}, asset.ID{1},
		1440, &address.RegressionNetTap)
	require.NoError(t, err)

	leaf, err := kit.GenTimeoutPathScript()
	require.NoError(t, err)
	require.Equal(t, "2079be667ef9dcbbac55a06295ce870b07029bfc"+
		"db2dce28d959f2815b16f81798ad02a005b2",
		hex.EncodeToString(leaf))
	plan := kit.contract.AnchorPlan()
	require.Equal(t, "023b46d262d2f610e9038b44beabdfe97ab5a0feb"+
		"89870acc2264edfb7f63ec2ec",
		hex.EncodeToString(plan.InternalKey[:]))
	var root swapkit.Hash
	for i := range root {
		root[i] = byte(0xa0 + i)
	}
	spend, err := kit.contract.TimeoutSpend(root)
	require.NoError(t, err)
	require.Equal(t, uint32(1440), spend.Sequence)
	require.Zero(t, spend.LockTime)
	require.Equal(t, "5120495c236364588781056c0d9f3fb69cadecd9628"+
		"f02b9b96ec07f2ebd2a9258f1",
		hex.EncodeToString(spend.PkScript))
	addressKey, err := kit.tapAddressScriptKey()
	require.NoError(t, err)
	require.Equal(t, "037bbbb85ff774141ee36c2cc9f38b40be4a02c2a"+
		"156c8a1def8781cf39177deca",
		hex.EncodeToString(addressKey.PubKey.SerializeCompressed()))
	assetKey, err := kit.TapScriptKey()
	require.NoError(t, err)
	require.Equal(t, "027bbbb85ff774141ee36c2cc9f38b40be4a02c2a"+
		"156c8a1def8781cf39177deca",
		hex.EncodeToString(assetKey.PubKey.SerializeCompressed()))
}
