package swap

import (
	"encoding/hex"
	"strconv"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	depositscript "github.com/lightninglabs/loop/staticaddr/script"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

// TestBitcoinLeafCompatibility preserves the leaf bytes produced before the
// Bitcoin and asset constructors shared their script serializers.
func TestBitcoinLeafCompatibility(t *testing.T) {
	_, key := btcec.PrivKeyFromBytes([]byte{1})
	const prefix = "2079be667ef9dcbbac55a06295ce870b07029bfcdb2d" +
		"ce28d959f2815b16f81798ad"
	for _, test := range []struct {
		expiry int64
		number string
	}{
		{-1, "4f"},
		{0, "00"},
		{1, "51"},
		{16, "60"},
		{17, "0111"},
		{127, "017f"},
		{128, "028000"},
		{65535, "03ffff00"},
		{65536, "03000001"},
		{499999999, "04ff64cd1d"},
		{500000000, "040065cd1d"},
	} {
		t.Run(strconv.FormatInt(test.expiry, 10), func(t *testing.T) {
			cltv, err := GenTimeoutPathScript(key, test.expiry)
			require.NoError(t, err)
			require.Equal(t, prefix+test.number+"b1",
				hex.EncodeToString(cltv))

			csv, err := depositscript.GenTimeoutPathScript(
				key, test.expiry,
			)
			require.NoError(t, err)
			require.Equal(t, prefix+test.number+"b2",
				hex.EncodeToString(csv))
		})
	}

	// The quote template deliberately uses a zero hash and negative expiry.
	// Shared serializers must not impose the asset constructor's admission
	// checks on these existing Bitcoin API inputs.
	success, err := GenSuccessPathScript(key, lntypes.Hash{})
	require.NoError(t, err)
	require.Equal(t, prefix+"82012088a914"+
		"d1a70126ff7a149ca6f9b638db084480440ff8428851b2",
		hex.EncodeToString(success))
	require.NotNil(t, QuoteHtlcP2TR)
}
