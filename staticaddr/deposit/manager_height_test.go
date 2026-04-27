package deposit

import (
	"testing"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/stretchr/testify/require"
)

// TestConfirmationHeightForUtxo verifies first-confirmation height lookup for
// wallet UTXOs.
func TestConfirmationHeightForUtxo(t *testing.T) {
	t.Run("unconfirmed", func(t *testing.T) {
		height, err := confirmationHeightForUtxo(0, &lnwallet.Utxo{})
		require.NoError(t, err)
		require.Zero(t, height)
	})

	t.Run("confirmed uses best height arithmetic", func(t *testing.T) {
		height, err := confirmationHeightForUtxo(101, &lnwallet.Utxo{
			Confirmations: 1,
			OutPoint: wire.OutPoint{
				Index: 1,
			},
		})
		require.NoError(t, err)
		require.EqualValues(t, 101, height)
	})

	t.Run("rejects impossible height", func(t *testing.T) {
		_, err := confirmationHeightForUtxo(2, &lnwallet.Utxo{
			Confirmations: 4,
			OutPoint: wire.OutPoint{
				Index: 3,
			},
		})
		require.ErrorContains(t, err, "invalid confirmation height")
	})
}
