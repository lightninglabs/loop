package tapsdk

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/swapkit"
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

// TestDepositInventoryRechecksConfirmation verifies that an old, spent deposit
// remains recognizable in normal inventory without a block-history scanner.
// A fresh driver still rejects a reorganized confirmation.
func TestDepositInventoryRechecksConfirmation(t *testing.T) {
	for _, reorg := range []bool{false, true} {
		name := "current"
		if reorg {
			name = "reorganized"
		}
		t.Run(name, func(t *testing.T) {
			r, swap, _, chain := bindingOutcome(t, false)
			record, err := r.localDeposit(t.Context(),
				swap.Offer.Terms.DepositOutpoints[0])
			require.NoError(t, err)
			chain.tip, chain.unspent = 1_000_000, false
			counted := &countedChain{bindingChain: chain}
			r.cfg.Chain = counted
			r = &Driver{cfg: r.cfg}
			if reorg {
				chain.blocks[100].Header.Nonce++
			}
			p, err := r.verifyDeposit(t.Context(), record.Deposit, false)
			if reorg {
				require.ErrorIs(t, err, swapkit.ErrPending)
				require.Nil(t, p)
			} else {
				require.NoError(t, err)
				require.Equal(t, record.Deposit.Outpoint,
					p.OutPoint().String())
			}
			expected := []uint32{100, 100}
			if reorg {
				expected = expected[:1]
			}
			require.Equal(t, expected, counted.heights)
		})
	}
}
