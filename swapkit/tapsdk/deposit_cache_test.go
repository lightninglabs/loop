package tapsdk

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/swapkit"
	sdk "github.com/lightninglabs/tap-sdk"
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
			p, err := r.verifyDepositContract(t.Context(),
				record.Deposit, record.Contract, false)
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

// countedProofSDK records externally verified lineages in one operation.
type countedProofSDK struct {
	*bindingSDK

	proofs [][]byte
}

func (s *countedProofSDK) VerifyProof(ctx context.Context,
	raw []byte) (*sdk.VerifyProofResponse, error) {

	s.proofs = append(s.proofs, raw)
	return s.bindingSDK.VerifyProof(ctx, raw)
}

// TestRefundOperationVerifiesEachProofOnce checks reuse within an operation,
// while a fresh operation after restart must revalidate both lineages.
func TestRefundOperationVerifiesEachProofOnce(t *testing.T) {
	r, swap, outcome, _ := originalRefundFixture(t)
	client := &countedProofSDK{bindingSDK: &bindingSDK{valid: true}}
	r.cfg.SDK = client
	evidence := bindingRaw(t, outcome)
	op := swapkit.Operation{Offer: swap.Offer, Evidence: evidence}
	for range 2 {
		client.proofs = nil
		r = &Driver{cfg: r.cfg}
		result, err := r.InspectRefund(t.Context(), op)
		require.NoError(t, err)
		require.True(t, result.Refunded)
		require.Equal(t, [][]byte{
			swap.Deposit.Deposit.Proof, evidence,
		}, client.proofs)
	}
}
