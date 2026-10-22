package htlc

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/commitment"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// TestIndependentKeyRoles exercises claims and refunds with four distinct
// keys in both asset directions. Cooperative keys cannot authorize scripts.
func TestIndependentKeyRoles(t *testing.T) {
	t.Run("CSV", func(t *testing.T) {
		testIndependentKeyRoles(t, AssetHTLCV0)
	})
	t.Run("CLTV", func(t *testing.T) {
		testIndependentKeyRoles(t, AssetHTLCCLTVV1)
	})
}

func testIndependentKeyRoles(t *testing.T, contractPolicy Policy) {
	t.Helper()

	for _, direction := range []struct {
		name    string
		reverse bool
	}{
		{name: "loop in"},
		{name: "loop out", reverse: true},
	} {
		t.Run(direction.name, func(t *testing.T) {
			for _, success := range []bool{false, true} {
				sequence := uint32(144)
				if contractPolicy == AssetHTLCCLTVV1 {
					sequence = wire.MaxTxInSequenceNum - 1
				}
				if success {
					sequence = SuccessSequence
				}
				f := newWitnessFixtureForPolicy(
					t, sequence, asset.V1,
					commitment.TapCommitmentV2, direction.reverse, contractPolicy,
				)
				if !success {
					f.packet.UnsignedTx.LockTime =
						f.kit.TimeoutSpendConstraints().LockTime
				}
				_, firstInternal := scalarKey(t, 3)
				_, secondInternal := scalarKey(t, 4)
				expected, err := input.MuSig2CombineKeys(
					input.MuSig2Version100RC2,
					[]*btcec.PublicKey{firstInternal, secondInternal},
					true, &input.MuSig2Tweaks{},
				)
				require.NoError(t, err)
				aggregate, err := f.kit.GetAggregateKey()
				require.NoError(t, err)
				require.True(t, aggregate.IsEqual(expected.PreTweakedKey))
				packet, err := f.kit.CreateHtlcVpkt()
				require.NoError(t, err)
				require.True(t, packet.Outputs[1].
					AnchorOutputInternalKey.IsEqual(aggregate))

				sign := func(signer *localSigner) (*SpendWitness, error) {
					if success {
						return f.kit.CreatePreimageWitness(
							t.Context(), signer, f.proof, f.packet,
							keychain.KeyLocator{}, f.preimage,
							f.transfer,
						)
					}

					return f.kit.CreateTimeoutWitness(
						t.Context(), signer, f.proof, f.packet,
						keychain.KeyLocator{}, f.transfer,
					)
				}
				for _, scalar := range []byte{3, 4} {
					internalPrivateKey, _ := scalarKey(t, scalar)
					_, err := sign(&localSigner{
						privateKey:    internalPrivateKey,
						expectedInput: f.assetInIndex,
					})
					require.ErrorContains(t, err, "wrong key")
				}
				privateKey := f.senderKey
				if success {
					privateKey = f.receiverKey
				}
				witness, err := sign(&localSigner{
					privateKey: privateKey, expectedInput: f.assetInIndex,
				})
				require.NoError(t, err)
				f.executeWitness(t, witness)
			}
		})
	}
}

// TestKeyRoleIsolation ensures constructor inputs and returned keys cannot
// change any of the four roles after the contract has been constructed.
func TestKeyRoleIsolation(t *testing.T) {
	params := vectorParams(t)
	_, params.SenderInternalPubKey = scalarKey(t, 3)
	_, params.ReceiverInternalPubKey = scalarKey(t, 4)
	kit, err := NewSwapKit(AssetHTLCV0, params)
	require.NoError(t, err)
	before, err := kit.CreateHtlcVpkt()
	require.NoError(t, err)
	root := make([]byte, 32)
	beforeScript, err := kit.GetPkScriptFromRoot(root)
	require.NoError(t, err)
	_, replacement := scalarKey(t, 42)
	for _, key := range []*btcec.PublicKey{
		params.SenderInternalPubKey, params.ReceiverInternalPubKey,
		params.SenderScriptKey, params.ReceiverScriptKey,
		kit.SenderInternalPubKey(), kit.ReceiverInternalPubKey(),
		kit.SenderScriptKey(), kit.ReceiverScriptKey(),
	} {
		*key = *replacement
	}
	after, err := kit.CreateHtlcVpkt()
	require.NoError(t, err)
	require.Equal(t, before, after)
	afterScript, err := kit.GetPkScriptFromRoot(root)
	require.NoError(t, err)
	require.Equal(t, beforeScript, afterScript)
}
