package deposit

import (
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/stretchr/testify/require"
)

// TestConfirmationHeightForUtxo verifies confirmation heights are derived from
// the current block height and wallet confirmation count.
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

	t.Run("snapshot unavailable", func(t *testing.T) {
		_, err := confirmationHeightForUtxo(0, &lnwallet.Utxo{
			Confirmations: 6,
		})
		require.ErrorContains(t, err, "confirmation tip height unavailable")
	})
}

// TestStableConfirmationTip verifies that confirmation counts are combined
// with an lnd height only when the wallet query is bracketed by the same synced
// chain tip.
func TestStableConfirmationTip(t *testing.T) {
	stableInfo := func() *lndclient.Info {
		return &lndclient.Info{
			BlockHeight:   200,
			BestBlockHash: chainhash.Hash{1},
			SyncedToChain: true,
		}
	}

	testCases := []struct {
		name   string
		before *lndclient.Info
		after  *lndclient.Info
		height uint32
	}{
		{
			name:   "stable",
			before: stableInfo(),
			after:  stableInfo(),
			height: 200,
		},
		{
			name: "catching up",
			before: &lndclient.Info{
				BlockHeight:   200,
				BestBlockHash: chainhash.Hash{1},
			},
			after: stableInfo(),
		},
		{
			name:   "height changed",
			before: stableInfo(),
			after: &lndclient.Info{
				BlockHeight:   201,
				BestBlockHash: chainhash.Hash{2},
				SyncedToChain: true,
			},
		},
		{
			name:   "block hash changed at same height",
			before: stableInfo(),
			after: &lndclient.Info{
				BlockHeight:   200,
				BestBlockHash: chainhash.Hash{2},
				SyncedToChain: true,
			},
		},
		{
			name:   "missing observation",
			before: stableInfo(),
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			height := stableConfirmationTip(
				testCase.before, testCase.after,
			)
			require.Equal(t, testCase.height, height)
		})
	}
}

// TestExpiryNotificationHeight verifies queued epochs cannot advance expiry
// beyond the authoritative lnd tip after a reorg or notification backlog.
func TestExpiryNotificationHeight(t *testing.T) {
	require.EqualValues(t, 199, expiryNotificationHeight(199, 200))
	require.EqualValues(t, 200, expiryNotificationHeight(201, 200))
}
