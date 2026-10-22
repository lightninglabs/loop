package tapsdk

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/loop/swapkit"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/stretchr/testify/require"
)

type headerOnlyChain struct {
	*bindingChain

	blockReads int
	hashReads  int
}

func (c *headerOnlyChain) Block(context.Context,
	uint32) (*wire.MsgBlock, error) {

	c.blockReads++
	return nil, swapkit.ErrInvalid
}

func (c *headerOnlyChain) BlockHash(ctx context.Context,
	height uint32) (chainhash.Hash, error) {

	c.hashReads++
	return c.bindingChain.BlockHash(ctx, height)
}

func TestConfirmationUsesCurrentHashAndMerkleProof(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*proof.Proof, *bindingChain)
		want   error
	}{
		{name: testValid},
		{name: "reorganized", want: swapkit.ErrPending,
			change: func(_ *proof.Proof, c *bindingChain) {
				c.blocks[100].Header.Nonce++
			}},
		{name: "wrong merkle path", want: swapkit.ErrInvalid,
			change: func(p *proof.Proof, _ *bindingChain) {
				p.TxMerkleProof.Nodes = []chainhash.Hash{{1}}
				p.TxMerkleProof.Bits = []bool{true}
			}},
		{name: "incomplete merkle path", want: swapkit.ErrInvalid,
			change: func(p *proof.Proof, _ *bindingChain) {
				p.TxMerkleProof.Nodes = []chainhash.Hash{{1}}
			}},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := bindingProof(t, bindingGenesis(), bindingKey(2).PubKey(),
				bindingKey(3).PubKey(), nil, bindingGenesis().FirstPrevOut,
				100, false)
			chain := &headerOnlyChain{bindingChain: &bindingChain{
				tip: 101, unspent: true,
				blocks: map[uint32]*wire.MsgBlock{100: bindingBlock(p)},
			}}
			if test.change != nil {
				test.change(p, chain.bindingChain)
			}
			for range 2 {
				err := checkConfirmation(t.Context(), chain, 2, p, true)
				if test.want == nil {
					require.NoError(t, err)
				} else {
					require.ErrorIs(t, err, test.want)
				}
			}
			require.Equal(t, 2, chain.hashReads)
			require.Zero(t, chain.blockReads)
		})
	}
}
