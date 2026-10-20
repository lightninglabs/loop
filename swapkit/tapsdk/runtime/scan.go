package runtime

import (
	"context"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/loop/swapkit/bridge"
)

// errScanIncomplete distinguishes a bounded historical scan from a complete
// scan with no match. Publication must not mistake yielding for absence.
var errScanIncomplete = errors.New("asset chain scan incomplete")

const scanBatchSize = 128

// scanProgress is a hint, never confirmation evidence. The checkpoint's hash
// commits to every scanned predecessor. A reorg invalidates that whole prefix.
type scanProgress struct {
	Height uint32
	Hash   chainhash.Hash
	Found  bool
}

func (r *Runtime) confirmation(ctx context.Context, txid chainhash.Hash,
	since uint32) (*wire.MsgBlock, uint32, error) {

	return r.scan(ctx, "tx/"+txid.String(), since,
		func(block *wire.MsgBlock) bool {
			for _, tx := range block.Transactions {
				if tx.TxHash() == txid {
					return true
				}
			}
			return false
		})
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

func (r *Runtime) findSpend(ctx context.Context, point wire.OutPoint,
	since uint32) (*wire.MsgBlock, uint32, error) {

	block, height, err := r.scan(ctx, "spend/"+point.String(), since,
		func(block *wire.MsgBlock) bool {
			return spendingTransaction(block, point) != nil
		})
	if errors.Is(err, errScanIncomplete) {
		err = bridge.ErrPending
	}
	return block, height, err
}

// scan retains progress even when a deadline interrupts a long recovery.
// Runtime.Call serializes access. Every block is checkpointed before advancing,
// including a match, so a restart also avoids rescanning completed searches.
func (r *Runtime) scan(ctx context.Context, target string, since uint32,
	matches func(*wire.MsgBlock) bool) (*wire.MsgBlock, uint32, error) {

	id := fmt.Sprintf("scan/v1/%s/%d", target, since)
	var progress scanProgress
	err := r.get(id, &progress)
	if err != nil && !missing(err) {
		return nil, 0, err
	}
	tip, err := r.cfg.Chain.Height(ctx)
	if err != nil {
		return nil, 0, err
	}
	next := uint64(since)
	if progress.Hash != (chainhash.Hash{}) && progress.Height <= tip {
		block, err := r.cfg.Chain.Block(ctx, progress.Height)
		if err != nil {
			return nil, 0, err
		}
		if block.BlockHash() == progress.Hash {
			if progress.Found {
				if !matches(block) {
					return nil, 0, bridge.ErrInvalid
				}
				return block, progress.Height, nil
			}
			next = uint64(progress.Height) + 1
		}
	}
	// A changed or shortened chain requires a complete rewind. Do not
	// assume a fixed reorg depth or reuse evidence from an orphaned branch.
	if next == uint64(since) {
		progress = scanProgress{}
		if err := r.put(id, progress); err != nil {
			return nil, 0, err
		}
	}
	for count := 0; next <= uint64(tip); count, next = count+1, next+1 {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		if count == scanBatchSize {
			return nil, 0, errScanIncomplete
		}
		block, err := r.cfg.Chain.Block(ctx, uint32(next))
		if err != nil {
			return nil, 0, err
		}
		// Detect a branch switch between checkpoint validation and the
		// next read. Never join a new block to an orphaned scan prefix.
		if progress.Hash != (chainhash.Hash{}) &&
			block.Header.PrevBlock != progress.Hash {

			if err := r.put(id, scanProgress{}); err != nil {
				return nil, 0, err
			}
			return nil, 0, errScanIncomplete
		}
		progress = scanProgress{
			Height: uint32(next), Hash: block.BlockHash(),
			Found: matches(block),
		}
		if err := r.put(id, progress); err != nil {
			return nil, 0, err
		}
		if progress.Found {
			return block, progress.Height, nil
		}
	}
	return nil, 0, bridge.ErrPending
}
