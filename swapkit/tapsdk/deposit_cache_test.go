package tapsdk

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/loop/swapkit"
	sdk "github.com/lightninglabs/tap-sdk"
	"github.com/stretchr/testify/require"
)

type countedChain struct {
	*bindingChain

	heights []uint32
}

func (c *countedChain) BlockHash(ctx context.Context,
	height uint32) (chainhash.Hash, error) {

	c.heights = append(c.heights, height)
	return c.bindingChain.BlockHash(ctx, height)
}

// TestDepositInventoryRechecksConfirmation verifies that an old, spent deposit
// remains recognizable in normal inventory without a block-history scanner.
// A later action still rejects a reorganized confirmation.
func TestDepositInventoryRechecksConfirmation(t *testing.T) {
	for _, reorg := range []bool{false, true} {
		name := "current"
		if reorg {
			name = "reorganized"
		}
		t.Run(name, func(t *testing.T) {
			r, swap, _, chain := bindingOutcome(t, false)
			record := swap.Deposit
			chain.tip, chain.unspent = 1_000_000, false
			counted := &countedChain{bindingChain: chain}
			r.chain = counted
			if reorg {
				chain.blocks[100].Header.Nonce++
			}
			err := VerifyDeposit(
				t.Context(),
				r.wallet(),
				counted,
				1,
				record,
				false,
			)
			if reorg {
				require.ErrorIs(t, err, swapkit.ErrPending)
			} else {
				require.NoError(t, err)
				require.Equal(t, record.Deposit.Outpoint,
					record.Proof.OutPoint().String())
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
	r.client = client
	evidence := bindingRaw(t, outcome)

	for range 2 {
		client.proofs = nil
		require.NoError(
			t,
			VerifyDeposit(
				t.Context(),
				r.wallet(),
				r.chain,
				1,
				swap.Deposit,
				false,
			),
		)
		require.NoError(
			t,
			VerifyRefundOutcome(
				t.Context(),
				r.wallet(),
				r.chain,
				1,
				swap,
				evidence,
			),
		)
		require.Equal(t, [][]byte{
			swap.Deposit.Deposit.Proof, evidence,
		}, client.proofs)
	}
}

// TestSDKBuilderOwnsInputVerification covers the real SDK builder call. The
// Loop checks on each side must not call verifyProof a second time, and the
// post-build check rejects chain changes during the SDK verification RPC.
func TestSDKBuilderOwnsInputVerification(t *testing.T) {
	for _, change := range []string{"none", testReorg, "mempool spend"} {
		t.Run(change, func(t *testing.T) {
			fixture := newCustomAnchorBuilderFixture(t)
			p, err := LastProof(fixture.request.Inputs[0].ProofFile)
			require.NoError(t, err)
			chain := &bindingChain{
				tip:     p.BlockHeight + 1,
				unspent: true,
				blocks: map[uint32]*wire.MsgBlock{
					p.BlockHeight: bindingBlock(p),
				},
			}
			fixture.client.afterVerify = func() {
				switch change {
				case testReorg:
					chain.blocks[p.BlockHeight].Header.Nonce++
				case "mempool spend":
					chain.unspent = false
				}
			}
			require.NoError(
				t,
				CheckInputsCurrent(t.Context(), chain, 1,
					fixture.request, true),
			)
			require.Zero(t, fixture.client.verifyCalls)
			wallet := sdk.NewWallet(
				fixture.client,
				sdk.NetworkRegtest,
			)
			_, err = wallet.NewCustomAnchorTxBuilder().
				Build(t.Context(), fixture.request)
			require.NoError(t, err)
			require.Equal(t, 1, fixture.client.verifyCalls)
			err = CheckInputsCurrent(
				t.Context(),
				chain,
				1,
				fixture.request,
				true,
			)
			if change == "none" {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, swapkit.ErrPending)
			}
			require.Equal(t, 1, fixture.client.verifyCalls)
		})
	}
}
