package htlc

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/stretchr/testify/require"
)

// TestReturnedNUMSKeysAreIndependent checks that changing a returned key cannot
// alter the process-wide NUMS point, other returned keys, or later contracts.
func TestReturnedNUMSKeysAreIndependent(t *testing.T) {
	for _, name := range []string{"control block", "raw key", "funding template"} {
		t.Run(name, func(t *testing.T) {
			original := *asset.NUMSPubKey
			t.Cleanup(func() { *asset.NUMSPubKey = original })
			expected := original.SerializeCompressed()
			before, err := GetOpTrueScriptKey()
			require.NoError(t, err)
			scriptKey, _, _, control, err := CreateOpTrueLeaf()
			require.NoError(t, err)
			kit := newVectorKit(t, AssetHTLCV0)
			packet, err := kit.CreateHtlcVpkt()
			require.NoError(t, err)
			keys := map[string]*btcec.PublicKey{
				"control block":    control.InternalKey,
				"raw key":          scriptKey.RawKey.PubKey,
				"funding template": packet.Outputs[0].ScriptKey.PubKey,
			}
			_, replacement := scalarKey(t, 42)
			*keys[name] = *replacement
			require.Equal(t, expected, asset.NUMSPubKey.SerializeCompressed())
			require.Equal(t, expected,
				asset.NUMSScriptKey.PubKey.SerializeCompressed())
			for other, key := range keys {
				if other != name {
					require.Equal(t, expected, key.SerializeCompressed())
				}
			}
			after, err := GetOpTrueScriptKey()
			require.NoError(t, err)
			require.Equal(t, before, after)
			next, err := kit.CreateHtlcVpkt()
			require.NoError(t, err)
			require.Equal(t, expected,
				next.Outputs[0].ScriptKey.PubKey.SerializeCompressed())
		})
	}
}
