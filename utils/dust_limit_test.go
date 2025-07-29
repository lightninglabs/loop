package utils

import (
	"testing"

	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/stretchr/testify/require"
)

type pkScriptGetter func([]byte) ([]byte, error)

// TestDustLimitForPkScript checks that the dust limit for a given script size
// matches the calculation in lnwallet.DustLimitForSize.
func TestDustLimitForPkScript(t *testing.T) {
	getScripts := map[int]pkScriptGetter{
		input.P2WPKHSize: input.WitnessPubKeyHash,
		input.P2WSHSize:  input.WitnessScriptHash,
		input.P2SHSize:   input.GenerateP2SH,
		input.P2PKHSize:  input.GenerateP2PKH,
	}

	for scriptSize, getPkScript := range getScripts {
		pkScript, err := getPkScript([]byte{})
		require.NoError(t, err, "failed to generate pkScript")

		require.Equal(
			t, lnwallet.DustLimitForSize(scriptSize),
			DustLimitForPkScript(pkScript),
		)
	}
}
