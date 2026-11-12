package tapsdk

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/swapkit"
	"github.com/stretchr/testify/require"
)

// reorgRaceChain keeps reporting the confirmed block hash, and serves the
// block that replaced it at the same height only to full block lookups. This
// models a reorg that lands after the last confirmation check.
type reorgRaceChain struct {
	*bindingChain

	height   uint32
	replaced *wire.MsgBlock
}

func (c *reorgRaceChain) Block(ctx context.Context,
	height uint32) (*wire.MsgBlock, error) {

	if height == c.height {
		return c.replaced, nil
	}
	return c.bindingChain.Block(ctx, height)
}

// TestVerifyOutcomeReorgIsRetryable checks that a reorg between the last
// confirmation check and the full block lookup of VerifyWalletSpend is
// reported as pending, like a reorg seen before any check, and not as a
// permanently invalid outcome.
func TestVerifyOutcomeReorgIsRetryable(t *testing.T) {
	for _, refund := range []bool{false, true} {
		name := hardeningClaimCase
		if refund {
			name = hardeningRefundCase
		}
		t.Run(name, func(t *testing.T) {
			checkReorgRace(t, refund)
		})
	}
}

func checkReorgRace(t *testing.T, refund bool) {
	t.Helper()
	r, s, p, chain := bindingOutcome(t, refund)
	raw := bindingRaw(t, p)

	// The untouched chain accepts the outcome.
	require.NoError(t, VerifyOutcome(t.Context(), r.wallet(), chain,
		1, s, raw, refund, refund))

	// A reorg observed before any check is classified as pending.
	original := chain.blocks[160]
	chain.blocks[160] = &wire.MsgBlock{
		Header: wire.BlockHeader{Nonce: 7},
	}
	err := VerifyOutcome(t.Context(), r.wallet(), chain, 1, s, raw,
		refund, refund)
	require.ErrorIs(t, err, swapkit.ErrPending)
	chain.blocks[160] = original

	// The same reorg observed only by the full block lookup, after
	// both confirmation checks passed, must stay pending as well.
	raced := &reorgRaceChain{bindingChain: chain, height: 160,
		replaced: &wire.MsgBlock{
			Header: wire.BlockHeader{Nonce: 7},
		}}
	err = VerifyOutcome(t.Context(), r.wallet(), raced, 1, s, raw,
		refund, refund)
	require.Error(t, err)
	require.ErrorIs(t, err, swapkit.ErrPending,
		"reorg race classified as: %v", err)
}
