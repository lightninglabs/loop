package tapsdk

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/swapkit"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/stretchr/testify/require"
)

type testChainKit struct {
	lndclient.ChainKitClient

	block *wire.MsgBlock
	hash  chainhash.Hash
}

func (k *testChainKit) GetBlockHash(context.Context, int64) (
	chainhash.Hash, error) {

	return k.hash, nil
}

func (k *testChainKit) GetBlock(context.Context, chainhash.Hash) (
	*wire.MsgBlock, error) {

	return k.block, nil
}

type testChainNotifier struct {
	lndclient.ChainNotifierClient

	spend   *chainntnfs.SpendDetail
	point   wire.OutPoint
	script  []byte
	hint    int32
	entered chan struct{}
}

func (n *testChainNotifier) RegisterSpendNtfn(_ context.Context,
	point *wire.OutPoint, script []byte, hint int32,
	_ ...lndclient.NotifierOption) (chan *chainntnfs.SpendDetail,
	chan error, error) {

	n.point, n.script, n.hint = *point, script, hint
	spends := make(chan *chainntnfs.SpendDetail, 1)
	if n.spend != nil {
		spends <- n.spend
	}
	if n.entered != nil {
		close(n.entered)
	}
	return spends, make(chan error), nil
}

func chainFixture() (*LNDChain, *testChainNotifier, wire.OutPoint) {
	point := wire.OutPoint{Hash: chainhash.Hash{7}, Index: 1}
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(wire.NewTxIn(&point, nil, nil))
	tx.AddTxOut(wire.NewTxOut(500, []byte{0x51}))
	block := &wire.MsgBlock{
		Header:       wire.BlockHeader{MerkleRoot: tx.TxHash()},
		Transactions: []*wire.MsgTx{tx},
	}
	n := &testChainNotifier{spend: &chainntnfs.SpendDetail{
		SpendingTx: tx, SpendingHeight: 200,
	}}
	return &LNDChain{
		Kit:      &testChainKit{block: block, hash: block.BlockHash()},
		Notifier: n,
	}, n, point
}

// TestSpendRecoveryUsesNativeNotifier restores the watch from its durable
// outpoint/script/height, without a second journal or scanning block history.
func TestSpendRecoveryUsesNativeNotifier(t *testing.T) {
	chain, notifier, point := chainFixture()
	for range 2 {
		block, height, err := chain.Spend(
			t.Context(), point, []byte{0x51}, 150,
		)
		require.NoError(t, err)
		require.EqualValues(t, 200, height)
		require.Equal(t, notifier.spend.SpendingTx,
			spendingTransaction(block, point))
		require.Equal(t, point, notifier.point)
		require.Equal(t, []byte{0x51}, notifier.script)
		require.EqualValues(t, 150, notifier.hint)
		// Recreate the adapter to model the normal daemon's restart.
		chain = &LNDChain{Kit: chain.Kit, Notifier: notifier}
	}
}

func TestSpendRecoveryRejectsStaleChainEvidence(t *testing.T) {
	chain, _, point := chainFixture()
	kit := chain.Kit.(*testChainKit)
	kit.block.Transactions = nil
	_, _, err := chain.Spend(t.Context(), point, []byte{0x51}, 150)
	require.ErrorIs(t, err, swapkit.ErrPending)

	kit.hash[0] ^= 1
	_, _, err = chain.Spend(t.Context(), point, []byte{0x51}, 150)
	require.ErrorIs(t, err, swapkit.ErrInvalid)
}

func TestSpendRecoveryHonorsShutdown(t *testing.T) {
	chain, notifier, point := chainFixture()
	notifier.spend, notifier.entered = nil, make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, _, err := chain.Spend(ctx, point, []byte{0x51}, 150)
		done <- err
	}()
	<-notifier.entered
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("spend recovery did not stop with its manager")
	}
}

// TestUnspentRequiresMempoolGate prevents wallet inventory or a confirmation
// notification being substituted for the current Bitcoin UTXO observation.
func TestUnspentRequiresMempoolGate(t *testing.T) {
	chain, _, point := chainFixture()
	_, err := chain.Unspent(t.Context(), point)
	require.ErrorIs(t, err, swapkit.ErrInvalid)
	for _, available := range []bool{true, false} {
		chain.UnspentOutput = func(_ context.Context,
			actual wire.OutPoint) (bool, error) {

			require.Equal(t, point, actual)
			return available, nil
		}
		found, err := chain.Unspent(t.Context(), point)
		require.NoError(t, err)
		require.Equal(t, available, found)
	}
}
