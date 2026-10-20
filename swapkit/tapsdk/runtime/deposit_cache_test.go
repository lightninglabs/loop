package runtime

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/loop/swapkit/bridge"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/stretchr/testify/require"
)

type countedChain struct {
	*bindingChain
	heights []uint32
}

func (c *countedChain) Block(ctx context.Context,
	height uint32) (*wire.MsgBlock, error) {

	c.heights = append(c.heights, height)
	return c.bindingChain.Block(ctx, height)
}

// TestDepositInventoryChecksCachedConfirmation verifies that an old, spent
// deposit remains in the exclusion inventory without scanning its whole
// history. A reorganized confirmation must never return the cached proof.
func TestDepositInventoryChecksCachedConfirmation(t *testing.T) {
	for _, reorg := range []bool{false, true} {
		name := "current"
		if reorg {
			name = "reorganized"
		}
		t.Run(name, func(t *testing.T) {
			r, swap, _, chain := bindingOutcome(t, false)
			record, err := r.localDeposit(
				swap.Offer.Terms.DepositOutpoints[0],
			)
			require.NoError(t, err)
			record.Request.RequestID = [32]byte{1}
			// Invalid final bytes stop reconstruction before any
			// wallet or broadcast effect. A current cached proof
			// never needs reconstruction.
			record.Transaction = confirmationPackage(t, asset.V1)
			record.Transaction.Final = []byte("invalid final")
			id := key("deposit/", record.Request.RequestID)
			require.NoError(t, r.put(id, record))
			chain.tip, chain.unspent = 1_000_000, false
			counted := &countedChain{bindingChain: chain}
			r.cfg.Chain = counted
			if reorg {
				chain.blocks[100].Header.Nonce++
			}
			deposit, err := r.createDeposit(
				t.Context(), record.Request,
			)
			if reorg {
				require.Error(t, err)
				require.Equal(t, bridge.Deposit{}, deposit)
			} else {
				require.NoError(t, err)
				require.Equal(t, record.Deposit, deposit)
			}
			expected := []uint32{100, 100}
			if reorg {
				expected = expected[:1]
			}
			require.Equal(t, expected, counted.heights)
		})
	}
}
