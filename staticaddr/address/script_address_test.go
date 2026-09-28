package address

import (
	"testing"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/lightninglabs/loop/test"
	"github.com/stretchr/testify/require"
)

// TestGetTaprootAddressFromScript checks network encoding against full address
// reconstruction and rejects malformed or non-Taproot scripts.
func TestGetTaprootAddressFromScript(t *testing.T) {
	_, client := test.CreateKey(1)
	_, server := test.CreateKey(2)
	for _, network := range []*chaincfg.Params{
		&chaincfg.MainNetParams, &chaincfg.TestNet3Params,
		&chaincfg.RegressionNetParams,
	} {
		t.Run(network.Name, func(t *testing.T) {
			manager, err := NewManager(&ManagerConfig{ChainParams: network}, 1)
			require.NoError(t, err)
			expected, err := manager.GetTaprootAddress(client, server, 144)
			require.NoError(t, err)
			script, err := txscript.PayToAddrScript(expected)
			require.NoError(t, err)
			actual, err := manager.GetTaprootAddressFromScript(script)
			require.NoError(t, err)
			require.Equal(t, expected.String(), actual.String())
			for _, invalid := range [][]byte{
				nil, script[:len(script)-1], append([]byte{0}, script[1:]...),
			} {
				_, err := manager.GetTaprootAddressFromScript(invalid)
				require.ErrorContains(t, err, "invalid static address P2TR script")
			}
		})
	}
}
