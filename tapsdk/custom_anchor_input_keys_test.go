package tapsdk

import (
	"bytes"
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/tappsbt"
	"github.com/lightninglabs/taproot-assets/tapscript"
	"github.com/lightninglabs/taproot-assets/tapsend"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// TestCustomAnchorReceivedKeyTweak proves that a wallet-owned unique script
// key needs its complete Taproot tweak, not only a recovered raw key locator.
// This is the same signing distinction between freshly minted BIP-86 assets
// and assets received at a unique Pedersen address.
func TestCustomAnchorReceivedKeyTweak(t *testing.T) {
	fixture := newCustomAnchorBuilderFixture(t)
	builder := NewWallet(fixture.client, NetworkRegtest).
		NewCustomAnchorTxBuilder()
	plan, err := builder.Build(t.Context(), fixture.request)
	require.NoError(t, err)
	packet, err := tappsbt.Decode(plan.ActiveVirtualPSBTs()[0])
	require.NoError(t, err)
	unique, err := asset.DeriveUniqueScriptKey(
		*fixture.assetSpendKey.PubKey(), packet.Inputs[0].Asset().ID(),
		asset.ScriptKeyDerivationUniquePedersen,
	)
	require.NoError(t, err)
	inputAsset := packet.Inputs[0].Asset().Copy()
	inputAsset.ScriptKey = asset.NewScriptKey(unique.PubKey)
	packet.SetInputAsset(0, inputAsset)
	input := packet.Inputs[0]
	input.PrevID.ScriptKey = asset.ToSerialized(unique.PubKey)
	input.Bip32Derivation = nil
	input.TaprootBip32Derivation = nil
	input.TaprootInternalKey = nil
	input.TaprootMerkleRoot = nil
	require.NoError(t, tapsend.PrepareOutputAssets(t.Context(), packet))
	raw, err := tappsbt.Encode(packet)
	require.NoError(t, err)

	// Emulate the backend's incomplete metadata recovery. Its BIP-86
	// signature must fail the actual asset VM against this unique key.
	incomplete, err := tappsbt.Decode(raw)
	require.NoError(t, err)
	derivation, tapDerivation := tappsbt.Bip32DerivationFromKeyDesc(
		keychain.KeyDescriptor{
			PubKey: fixture.assetSpendKey.PubKey(),
		}, 1,
	)
	incomplete.Inputs[0].Bip32Derivation = []*psbt.Bip32Derivation{
		derivation,
	}
	incomplete.Inputs[0].TaprootBip32Derivation =
		[]*psbt.TaprootBip32Derivation{tapDerivation}
	err = tapsend.SignVirtualTransaction(
		incomplete, tapscript.NewMockSigner(fixture.assetSpendKey),
		customAnchorTestWitnessValidator{},
	)
	require.ErrorContains(t, err, "invalid transfer asset witness")

	resolved := ScriptKey{
		PubKey: mustCustomAnchorPubKey(t, unique.PubKey),
		KeyDesc: KeyDescriptor{
			RawKeyBytes: mustCustomAnchorPubKey(
				t, fixture.assetSpendKey.PubKey(),
			),
		},
		TapTweak: bytes.Clone(unique.Tweak),
	}
	fixture.client.queryScriptKey = func(_ context.Context,
		key []byte) (*ScriptKey, error) {

		require.Equal(t, schnorr.SerializePubKey(unique.PubKey), key)
		return &resolved, nil
	}
	require.NoError(t, builder.resolveInputSigningKeys(t.Context(), packet))
	require.Equal(t, unique.Tweak, packet.Inputs[0].TaprootMerkleRoot)
	encoded, err := tappsbt.Encode(packet)
	require.NoError(t, err)
	hydrated, err := tappsbt.Decode(encoded)
	require.NoError(t, err)
	require.NoError(t, tapsend.SignVirtualTransaction(
		hydrated, tapscript.NewMockSigner(fixture.assetSpendKey),
		customAnchorTestWitnessValidator{},
	))

	for _, test := range []struct {
		name   string
		mutate func(*ScriptKey) *ScriptKey
	}{
		{"missing key", func(*ScriptKey) *ScriptKey { return nil }},
		{"missing raw key", func(k *ScriptKey) *ScriptKey {
			k.KeyDesc.RawKeyBytes = PubKey{}
			return k
		}},
		{"missing tweak", func(k *ScriptKey) *ScriptKey {
			k.TapTweak = nil
			return k
		}},
		{"wrong tweak", func(k *ScriptKey) *ScriptKey {
			k.TapTweak[0] ^= 1
			return k
		}},
		{"wrong proof key", func(k *ScriptKey) *ScriptKey {
			k.PubKey = mustCustomAnchorPubKey(t,
				testPrivateKey(t, 99).PubKey())
			return k
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := resolved
			candidate.TapTweak = bytes.Clone(resolved.TapTweak)
			fixture.client.queryScriptKey = func(context.Context,
				[]byte) (*ScriptKey, error) {

				return test.mutate(&candidate), nil
			}
			fresh, err := tappsbt.Decode(raw)
			require.NoError(t, err)
			require.Error(t, builder.resolveInputSigningKeys(
				t.Context(), fresh,
			))
		})
	}
}
