package tapsdk

import (
	"context"
	"math"
	"time"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/swapkit"
)

// Chain uses the daemon's chain services. Unspent includes mempool spends;
// notifications may be restored from their original height hints after restart.
type Chain interface {
	Height(context.Context) (uint32, error)
	Unspent(context.Context, wire.OutPoint) (bool, error)
	Block(context.Context, uint32) (*wire.MsgBlock, error)
	Spend(context.Context, wire.OutPoint, []byte, uint32) (
		*wire.MsgBlock, uint32, error)
}

// LNDChain reuses the normal daemon's authenticated LND services. The UTXO
// callback must query its Bitcoin backend with mempool lookup enabled: wallet
// ListUnspent does not cover every externally controlled asset anchor.
type LNDChain struct {
	Kit           lndclient.ChainKitClient
	Notifier      lndclient.ChainNotifierClient
	UnspentOutput func(context.Context, wire.OutPoint) (bool, error)
}

// Height returns LND's current best-chain height.
func (c *LNDChain) Height(ctx context.Context) (uint32, error) {
	_, height, err := c.Kit.GetBestBlock(ctx)
	if err != nil {
		return 0, err
	}
	if height < 0 {
		return 0, swapkit.ErrInvalid
	}
	return uint32(height), nil
}

// Unspent checks the current chain and mempool through the configured backend.
func (c *LNDChain) Unspent(ctx context.Context,
	point wire.OutPoint) (bool, error) {

	if c.UnspentOutput == nil {
		return false, swapkit.ErrInvalid
	}
	return c.UnspentOutput(ctx, point)
}

// Block obtains a block from the current best chain, checking its identity.
func (c *LNDChain) Block(ctx context.Context,
	height uint32) (*wire.MsgBlock, error) {

	hash, err := c.Kit.GetBlockHash(ctx, int64(height))
	if err != nil {
		return nil, err
	}
	block, err := c.Kit.GetBlock(ctx, hash)
	if err != nil {
		return nil, err
	}
	if block == nil || block.BlockHash() != hash {
		return nil, swapkit.ErrInvalid
	}
	return block, nil
}

// Spend restores a spend watch from the persisted outpoint and height hint.
// The returned transaction is checked against the current best-chain block;
// a stale notifier response never becomes confirmation evidence.
func (c *LNDChain) Spend(ctx context.Context, point wire.OutPoint,
	pkScript []byte, hint uint32) (*wire.MsgBlock, uint32, error) {

	if hint > math.MaxInt32 || len(pkScript) == 0 {
		return nil, 0, swapkit.ErrInvalid
	}
	probe, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	spent, failed, err := c.Notifier.RegisterSpendNtfn(
		probe, &point, pkScript, int32(hint),
	)
	if err != nil {
		return nil, 0, err
	}
	select {
	case detail, ok := <-spent:
		if !ok || detail == nil || detail.SpendingTx == nil ||
			detail.SpendingHeight < 0 {

			return nil, 0, swapkit.ErrInvalid
		}
		height := uint32(detail.SpendingHeight)
		block, err := c.Block(ctx, height)
		if err != nil {
			return nil, 0, err
		}
		transaction := spendingTransaction(block, point)
		if transaction == nil ||
			transaction.TxHash() != detail.SpendingTx.TxHash() {

			return nil, 0, swapkit.ErrPending
		}
		return block, height, nil
	case err := <-failed:
		if err == nil {
			err = swapkit.ErrPending
		}
		return nil, 0, err
	case <-probe.Done():
		if ctx.Err() != nil {
			return nil, 0, ctx.Err()
		}
		return nil, 0, swapkit.ErrPending
	}
}

func spendingTransaction(block *wire.MsgBlock,
	point wire.OutPoint) *wire.MsgTx {

	for _, tx := range block.Transactions {
		for _, input := range tx.TxIn {
			if input.PreviousOutPoint == point {
				return tx
			}
		}
	}
	return nil
}
