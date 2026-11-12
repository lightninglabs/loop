package tapsdk

import (
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/swapkit"
	"github.com/stretchr/testify/require"
)

// TestDiscoverOriginalRefundSurvivesStaleBlock checks that a Chain which
// returns a block without the watched spend yields a retryable error instead
// of dereferencing a nil transaction.
func TestDiscoverOriginalRefundSurvivesStaleBlock(t *testing.T) {
	node, swap, _, chain := originalRefundFixture(t)
	foreign := wire.NewMsgTx(2)
	foreign.AddTxIn(wire.NewTxIn(
		&wire.OutPoint{Hash: chainhash.Hash{0xaa}}, nil, nil,
	))
	foreign.AddTxOut(wire.NewTxOut(1, []byte{txscript.OP_TRUE}))
	pc := &partialRecoveryChain{
		bindingChain: chain,
		spend: func(wire.OutPoint) (*wire.MsgBlock, uint32, error) {
			return &wire.MsgBlock{
				Transactions: []*wire.MsgTx{foreign},
			}, 201, nil
		},
	}
	_, err := DiscoverOriginalRefund(
		t.Context(), node.wallet(), pc, node.minConfs, swap,
	)
	require.ErrorIs(t, err, swapkit.ErrPending)
}

// TestDiscoverOriginalRefundRejectsAnnexSpend checks that an observed refund
// carrying a BIP-341 annex is identified as the agreed leaf but not
// reconstructed through a mis-sliced witness.
func TestDiscoverOriginalRefundRejectsAnnexSpend(t *testing.T) {
	node, swap, refund, chain := originalRefundFixture(t)
	annexed := refund.AnchorTx.Copy()
	annexed.TxIn[0].Witness = append(annexed.TxIn[0].Witness,
		[]byte{txscript.TaprootAnnexTag})
	pc := &partialRecoveryChain{
		bindingChain: chain,
		spend: func(wire.OutPoint) (*wire.MsgBlock, uint32, error) {
			return &wire.MsgBlock{
				Header:       refund.BlockHeader,
				Transactions: []*wire.MsgTx{annexed},
			}, refund.BlockHeight, nil
		},
	}
	// spendsLeaf strips the annex and matches the agreed leaf, so the refund
	// reaches reconstruction, which rejects the annex rather than mis-slice.
	require.True(t, spendsLeaf(annexed, swap.Deposit.Contract.Output,
		mustOriginalSpend(t, swap)))
	_, err := DiscoverOriginalRefund(
		t.Context(), node.wallet(), pc, node.minConfs, swap,
	)
	require.ErrorIs(t, err, swapkit.ErrInvalid)
}

func mustOriginalSpend(t *testing.T,
	swap *SwapContext) *swapkit.SwapAnchorSpendInfo {

	t.Helper()
	_, spend, err := OriginalRefundPolicy(swap.Deposit)
	require.NoError(t, err)
	return spend
}
