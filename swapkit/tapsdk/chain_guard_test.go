package tapsdk

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/loop/swapkit"
	"github.com/stretchr/testify/require"
)

type currentChainKit struct {
	*testChainKit

	height int32
}

func (k *currentChainKit) GetBestBlock(context.Context) (
	chainhash.Hash, int32, error) {

	return k.hash, k.height, nil
}

func TestLNDChainReadsCurrentBackend(t *testing.T) {
	chain, _, point := chainFixture()
	kit := &currentChainKit{
		testChainKit: chain.Kit.(*testChainKit), height: 200,
	}
	chain.Kit = kit
	for _, height := range []int32{200, 201} {
		kit.height = height
		actual, err := chain.Height(t.Context())
		require.NoError(t, err)
		require.EqualValues(t, height, actual)
	}
	block, err := chain.Block(t.Context(), 200)
	require.NoError(t, err)
	previousHash := block.BlockHash()
	kit.block.Header.Nonce++
	kit.hash = kit.block.BlockHash()
	block, err = chain.Block(t.Context(), 200)
	require.NoError(t, err)
	require.NotEqual(t, previousHash, block.BlockHash())

	for _, available := range []bool{true, false} {
		chain.UnspentOutput = func(_ context.Context,
			actual wire.OutPoint) (bool, error) {

			require.Equal(t, point, actual)
			return available, nil
		}
		chain.PrevOutput = func(_ context.Context,
			actual wire.OutPoint) (*wire.TxOut, error) {

			require.Equal(t, point, actual)
			if !available {
				return nil, nil
			}
			return wire.NewTxOut(500, []byte{0x51}), nil
		}
		unspent, err := chain.Unspent(t.Context(), point)
		require.NoError(t, err)
		require.Equal(t, available, unspent)
		// A full mempool-aware prevout lookup is enough to configure both
		// current funding authentication and current spendability.
		chain.UnspentOutput = nil
		unspent, err = chain.Unspent(t.Context(), point)
		require.NoError(t, err)
		require.Equal(t, available, unspent)
		previous, err := chain.Prevout(t.Context(), point)
		require.NoError(t, err)
		if available {
			require.Equal(t, int64(500), previous.Value)
		} else {
			require.Nil(t, previous)
		}
	}
}

func TestLNDChainRejectsMissingServices(t *testing.T) {
	var missing *LNDChain
	for _, chain := range []*LNDChain{missing, {}} {
		_, err := chain.Height(t.Context())
		require.ErrorIs(t, err, swapkit.ErrInvalid)
		_, err = chain.Block(t.Context(), 1)
		require.ErrorIs(t, err, swapkit.ErrInvalid)
		_, err = chain.BlockHash(t.Context(), 1)
		require.ErrorIs(t, err, swapkit.ErrInvalid)
		_, err = chain.Unspent(t.Context(), wire.OutPoint{})
		require.ErrorIs(t, err, swapkit.ErrInvalid)
		_, err = chain.Prevout(t.Context(), wire.OutPoint{})
		require.ErrorIs(t, err, swapkit.ErrInvalid)
		_, _, err = chain.Spend(t.Context(), wire.OutPoint{}, []byte{0x51}, 1)
		require.ErrorIs(t, err, swapkit.ErrInvalid)
	}
}
