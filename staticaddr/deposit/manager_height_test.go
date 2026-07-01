package deposit

import (
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/stretchr/testify/require"
)

func TestConfirmationHeightForUtxo(t *testing.T) {
	t.Run("unconfirmed", func(t *testing.T) {
		height, err := confirmationHeightForUtxo(0, &lnwallet.Utxo{})
		require.NoError(t, err)
		require.Zero(t, height)
	})

	t.Run("confirmed", func(t *testing.T) {
		height, err := confirmationHeightForUtxo(101, &lnwallet.Utxo{
			OutPoint: wire.OutPoint{
				Hash:  chainhash.Hash{1},
				Index: 2,
			},
			Confirmations: 6,
		})
		require.NoError(t, err)
		require.EqualValues(t, 96, height)
	})

	t.Run("invalid current height", func(t *testing.T) {
		_, err := confirmationHeightForUtxo(2, &lnwallet.Utxo{
			Confirmations: 6,
		})
		require.ErrorContains(t, err, "invalid confirmation height")
	})
}
