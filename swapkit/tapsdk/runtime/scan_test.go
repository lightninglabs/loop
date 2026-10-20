package runtime

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/loop/swapkit/bridge"
	"github.com/stretchr/testify/require"
	bolt "go.etcd.io/bbolt"
)

type scanChain struct {
	*bindingChain
	reads  []uint32
	budget int
	before func(uint32)
}

func (c *scanChain) Block(ctx context.Context,
	height uint32) (*wire.MsgBlock, error) {

	if c.budget == 0 {
		return nil, context.DeadlineExceeded
	}
	c.budget--
	c.reads = append(c.reads, height)
	if c.before != nil {
		c.before(height)
	}
	return c.bindingChain.Block(ctx, height)
}

func scanFixture(t *testing.T, tip uint32) (*scanChain, string) {
	t.Helper()
	c := &scanChain{bindingChain: &bindingChain{
		tip: tip, blocks: make(map[uint32]*wire.MsgBlock),
	}, budget: 10000}
	for height := uint32(0); height <= tip; height++ {
		c.blocks[height] = &wire.MsgBlock{
			Header: wire.BlockHeader{Nonce: height},
		}
	}
	relinkScanChain(c, 0)
	return c, filepath.Join(t.TempDir(), "runtime.db")
}

func relinkScanChain(c *scanChain, since uint32) {
	for height := since; height <= c.tip; height++ {
		if height > 0 {
			c.blocks[height].Header.PrevBlock =
				c.blocks[height-1].BlockHash()
		}
	}
}

func openScanRuntime(t *testing.T, path string, chain Chain) *Runtime {
	t.Helper()
	db, err := bolt.Open(path, 0600, nil)
	require.NoError(t, err)
	require.NoError(t, db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists([]byte("runtime-v1"))
		return err
	}))
	return &Runtime{cfg: Config{Chain: chain}, db: db}
}

// TestScanProgressSurvivesRestarts bounds each recovery attempt and verifies
// that process death and action timeouts cannot repeatedly strand it at the
// beginning of a long history. Both tx confirmation and spend discovery use
// this journal.
func TestScanProgressSurvivesRestarts(t *testing.T) {
	c, path := scanFixture(t, 400)
	point := wire.OutPoint{Hash: chainhash.Hash{7}}
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(wire.NewTxIn(&point, nil, nil))
	c.blocks[350].Transactions = []*wire.MsgTx{tx}
	c.blocks[350].Header.MerkleRoot = tx.TxHash()
	relinkScanChain(c, 350)

	r := openScanRuntime(t, path, c)
	c.budget = 40
	_, _, err := r.confirmation(t.Context(), tx.TxHash(), 0)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.NoError(t, r.Close())
	for attempt := 0; ; attempt++ {
		require.Less(t, attempt, 4)
		r = openScanRuntime(t, path, c)
		c.budget, c.reads = 10000, nil
		block, height, err := r.confirmation(
			t.Context(), tx.TxHash(), 0,
		)
		require.LessOrEqual(t, len(c.reads), scanBatchSize+1)
		require.GreaterOrEqual(t, c.reads[0], uint32(39))
		require.NoError(t, r.Close())
		if err == nil {
			require.EqualValues(t, 350, height)
			require.Equal(t, tx.TxHash(),
				block.Transactions[0].TxHash())
			break
		}
		require.ErrorIs(t, err, errScanIncomplete)
	}
	r = openScanRuntime(t, path, c)
	defer r.Close()
	c.reads = nil
	_, height, err := r.confirmation(t.Context(), tx.TxHash(), 0)
	require.NoError(t, err)
	require.EqualValues(t, 350, height)
	require.Equal(t, []uint32{350}, c.reads)
	for attempt := 0; ; attempt++ {
		require.Less(t, attempt, 4)
		block, height, err := r.findSpend(t.Context(), point, 0)
		if err == nil {
			require.EqualValues(t, 350, height)
			require.Equal(t, tx, spendingTransaction(block, point))
			break
		}
		require.ErrorIs(t, err, bridge.ErrPending)
	}
}

// TestScanReorgRewinds checks negative and positive caches, as well as a
// branch change between validating a checkpoint and reading its successor.
func TestScanReorgRewinds(t *testing.T) {
	for _, mode := range []string{
		"absent", "found", "during scan", "shorter chain",
	} {
		t.Run(mode, func(t *testing.T) {
			c, path := scanFixture(t, 90)
			r := openScanRuntime(t, path, c)
			t.Cleanup(func() { _ = r.Close() })
			tx := wire.NewMsgTx(2)
			tx.AddTxIn(&wire.TxIn{Sequence: 7})
			if mode == "found" || mode == "shorter chain" {
				c.blocks[70].Transactions = []*wire.MsgTx{tx}
				c.blocks[70].Header.MerkleRoot = tx.TxHash()
				relinkScanChain(c, 70)
			}
			if mode == "during scan" {
				c.budget = 40
			}
			_, _, err := r.confirmation(t.Context(), tx.TxHash(), 0)
			switch mode {
			case "found", "shorter chain":
				require.NoError(t, err)
			case "absent":
				require.ErrorIs(t, err, bridge.ErrPending)
			default:
				require.ErrorIs(
					t, err, context.DeadlineExceeded,
				)
			}
			reorg := func() {
				c.blocks[70].Transactions = nil
				c.blocks[20].Transactions = []*wire.MsgTx{tx}
				c.blocks[20].Header.MerkleRoot = tx.TxHash()
				relinkScanChain(c, 20)
			}
			c.budget = 10000
			if mode == "during scan" {
				c.before = func(height uint32) {
					if height == 40 {
						reorg()
						c.before = nil
					}
				}
				_, _, err = r.confirmation(
					t.Context(), tx.TxHash(), 0,
				)
				require.ErrorIs(t, err, errScanIncomplete)
			} else {
				reorg()
			}
			if mode == "shorter chain" {
				c.tip = 30
			}
			require.NoError(t, r.Close())
			r = openScanRuntime(t, path, c)
			block, height, err := r.confirmation(
				t.Context(), tx.TxHash(), 0,
			)
			require.NoError(t, err)
			require.EqualValues(t, 20, height)
			require.Equal(t, tx, block.Transactions[0])
		})
	}
}
