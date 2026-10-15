package deposit

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/loop/assets/htlc"
	"github.com/lightninglabs/taproot-assets/address"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

// TestIndependentKeyRoles checks deposit and derived HTLC contracts use the
// same four key roles in either on-chain asset direction.
func TestIndependentKeyRoles(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		_, senderScript := scalarKey(t, 1)
		_, receiverScript := scalarKey(t, 2)
		_, senderInternal := scalarKey(t, 3)
		_, receiverInternal := scalarKey(t, 4)
		if reverse {
			senderScript, receiverScript = receiverScript, senderScript
			senderInternal, receiverInternal = receiverInternal, senderInternal
		}
		genesis := testAddressGenesis()
		params := Params{
			SenderInternalPubKey:   senderInternal,
			ReceiverInternalPubKey: receiverInternal,
			SenderScriptKey:        senderScript,
			ReceiverScriptKey:      receiverScript,
			AssetID:                genesis.ID(), Genesis: &genesis, CsvExpiry: 144,
			ChainParams: &address.RegressionNetTap,
		}
		kit, err := NewKit(params)
		require.NoError(t, err)
		expected, err := input.MuSig2CombineKeys(
			input.MuSig2Version100RC2,
			[]*btcec.PublicKey{senderInternal, receiverInternal},
			true, &input.MuSig2Tweaks{},
		)
		require.NoError(t, err)
		require.True(t, kit.muSig2Key.PreTweakedKey.
			IsEqual(expected.PreTweakedKey))
		timeout, err := kit.GenTimeoutPathScript()
		require.NoError(t, err)
		expectedTimeout, err := htlc.GenTimeoutPathScript(senderScript, 144)
		require.NoError(t, err)
		require.Equal(t, expectedTimeout, timeout)

		client := &versionAddressClient{genesis: genesis}
		_, err = kit.NewAddr(t.Context(), client, 1000)
		require.NoError(t, err)
		require.Equal(t, expected.PreTweakedKey.SerializeCompressed(),
			client.request.InternalKey.RawKeyBytes)
		swapHash := lntypes.Hash{1}
		_, swapKit, err := kit.NewHtlcAddr(
			t.Context(), client, 1000, swapHash, 144,
		)
		require.NoError(t, err)
		require.Equal(t, expected.PreTweakedKey.SerializeCompressed(),
			client.request.InternalKey.RawKeyBytes)
		success, err := swapKit.GetSuccessScript()
		require.NoError(t, err)
		expectedSuccess, err := htlc.GenSuccessPathScript(
			receiverScript, swapHash,
		)
		require.NoError(t, err)
		require.Equal(t, expectedSuccess, success)
		htlcTimeout, err := swapKit.GetTimeoutScript()
		require.NoError(t, err)
		require.Equal(t, expectedTimeout, htlcTimeout)
	}
}

// TestTimeoutRejectsInternalKeys ensures cooperative key holders cannot use
// those keys in place of the sender's distinct refund script key.
func TestTimeoutRejectsInternalKeys(t *testing.T) {
	for _, scalar := range []byte{3, 4} {
		f := newWitnessFixture(t)
		privateKey, _ := scalarKey(t, scalar)
		_, err := f.kit.CreateTimeoutWitness(
			t.Context(), &localSigner{
				privateKey: privateKey, expectedInput: f.assetInIndex,
			}, f.proof, f.packet, f.transfer,
		)
		require.ErrorContains(t, err, "wrong key")
	}
}

// TestKeyRoleIsolation verifies constructor keys and keys returned by a derived
// HTLC cannot mutate the deposit contract or its future HTLC derivations.
func TestKeyRoleIsolation(t *testing.T) {
	_, senderInternal := scalarKey(t, 3)
	_, receiverInternal := scalarKey(t, 4)
	_, senderScript := scalarKey(t, 1)
	_, receiverScript := scalarKey(t, 2)
	genesis := testAddressGenesis()
	params := Params{
		SenderInternalPubKey: senderInternal, ReceiverInternalPubKey: receiverInternal,
		SenderScriptKey: senderScript, ReceiverScriptKey: receiverScript,
		AssetID: genesis.ID(), Genesis: &genesis, CsvExpiry: 144,
		ChainParams: &address.RegressionNetTap,
	}
	kit, err := NewKit(params)
	require.NoError(t, err)
	before, err := kit.newHtlcSwapKit(1000, lntypes.Hash{1}, 144)
	require.NoError(t, err)
	root := make([]byte, 32)
	beforeScript, err := before.GetPkScriptFromRoot(root)
	require.NoError(t, err)
	beforeRefund, err := kit.GenTimeoutPathScript()
	require.NoError(t, err)
	_, replacement := scalarKey(t, 42)
	for _, key := range []*btcec.PublicKey{
		senderInternal, receiverInternal, senderScript, receiverScript,
		before.SenderInternalPubKey(), before.ReceiverInternalPubKey(),
		before.SenderScriptKey(), before.ReceiverScriptKey(),
	} {
		*key = *replacement
	}
	after, err := kit.newHtlcSwapKit(1000, lntypes.Hash{1}, 144)
	require.NoError(t, err)
	afterScript, err := after.GetPkScriptFromRoot(root)
	require.NoError(t, err)
	require.Equal(t, beforeScript, afterScript)
	afterRefund, err := kit.GenTimeoutPathScript()
	require.NoError(t, err)
	require.Equal(t, beforeRefund, afterRefund)
	client := &versionAddressClient{genesis: genesis}
	_, _, err = kit.NewHtlcAddr(t.Context(), client, 1000, lntypes.Hash{1}, 144)
	require.NoError(t, err)
	require.Equal(t, kit.muSig2Key.PreTweakedKey.SerializeCompressed(),
		client.request.InternalKey.RawKeyBytes)
}
