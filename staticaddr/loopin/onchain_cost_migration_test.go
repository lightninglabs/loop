package loopin

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/wallet"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

// TestMigrateOnchainCost tests that the migration sets the on-chain cost of
// past swaps that consumed their deposits.
func TestMigrateOnchainCost(t *testing.T) {
	c := newOnchainCostTestContext(t)

	// Two swaps use the deposits of one funding transaction, a third swap
	// uses an externally funded deposit.
	shared := fundingTx(
		1, staticOut(100_000), otherOut(500_000), staticOut(100_000),
	)
	external := fundingTx(2, staticOut(100_000))
	single := fundingTx(3, otherOut(10_000), staticOut(100_000))
	pendingTx := fundingTx(4, staticOut(100_000))
	failedTx := fundingTx(5, staticOut(100_000))

	mockLnd := test.NewMockLnd()
	mockLnd.Transactions = []lndclient.Transaction{
		walletTx(shared, 1_001, true),
		walletTx(external, 2_000, false),
		walletTx(single, 700, true),
		walletTx(pendingTx, 300, true),
		walletTx(failedTx, 400, true),
	}

	succeeded := c.createLoopIn(
		lntypes.Hash{0x1}, Succeeded,
		wire.OutPoint{Hash: shared.TxHash(), Index: 0},
	)
	timedOut := c.createLoopIn(
		lntypes.Hash{0x2}, HtlcTimeoutSwept,
		wire.OutPoint{Hash: shared.TxHash(), Index: 2},
	)
	unknown := c.createLoopIn(
		lntypes.Hash{0x3}, Succeeded,
		wire.OutPoint{Hash: external.TxHash(), Index: 0},
	)
	paid := c.createLoopIn(
		lntypes.Hash{0x4}, PaymentReceived,
		wire.OutPoint{Hash: single.TxHash(), Index: 1},
	)
	pending := c.createLoopIn(
		lntypes.Hash{0x5}, MonitorInvoiceAndHtlcTx,
		wire.OutPoint{Hash: pendingTx.TxHash(), Index: 0},
	)
	failed := c.createLoopIn(
		lntypes.Hash{0x6}, Failed,
		wire.OutPoint{Hash: failedTx.TxHash(), Index: 0},
	)

	// A swap that already has a cost keeps it.
	recorded := c.createLoopIn(
		lntypes.Hash{0x7}, Succeeded,
		wire.OutPoint{Hash: single.TxHash(), Index: 1},
	)
	recordedCost := btcutil.Amount(42)
	recorded.OnchainCost = &recordedCost
	require.NoError(t, c.swapStore.UpdateLoopIn(t.Context(), recorded))

	// A swap whose outpoints can't be parsed is skipped for good, so it
	// doesn't keep the migration from completing.
	id, err := deposit.GetRandomDepositID()
	require.NoError(t, err)
	malformedDeposit := &deposit.Deposit{
		ID:                   id,
		OutPoint:             wire.OutPoint{Hash: chainhash.Hash{0x8}},
		Value:                btcutil.Amount(100_000),
		ConfirmationHeight:   100,
		TimeOutSweepPkScript: []byte{0x00, 0x14, 0x1a},
	}
	require.NoError(
		t, c.depositStore.CreateDeposit(t.Context(), malformedDeposit),
	)
	malformed := c.createLoopInWithOutpoints(
		lntypes.Hash{0x8}, Succeeded,
		[]*deposit.Deposit{malformedDeposit}, []string{"malformed"},
	)

	err = MigrateOnchainCost(
		t.Context(), c.db, mockLnd.Client, mockLnd.WalletKit,
		c.depositStore, c.swapStore,
	)
	require.NoError(t, err)

	// The deposits of the shared funding transaction split its fee, so it
	// is only counted once across both swaps.
	require.Equal(t, btcutil.Amount(501), *c.onchainCost(succeeded.SwapHash))
	require.Equal(t, btcutil.Amount(500), *c.onchainCost(timedOut.SwapHash))
	require.Equal(t, btcutil.Amount(700), *c.onchainCost(paid.SwapHash))
	require.Equal(t, recordedCost, *c.onchainCost(recorded.SwapHash))

	// Unknown fees and swaps that didn't consume their deposits have no
	// cost.
	require.Nil(t, c.onchainCost(unknown.SwapHash))
	require.Nil(t, c.onchainCost(pending.SwapHash))
	require.Nil(t, c.onchainCost(failed.SwapHash))
	require.Nil(t, c.onchainCost(malformed.SwapHash))

	// The migration only runs once.
	done, err := c.db.HasMigration(t.Context(), onchainCostMigrationID)
	require.NoError(t, err)
	require.True(t, done)

	mockLnd.Transactions = []lndclient.Transaction{
		walletTx(external, 2_000, true),
	}
	err = MigrateOnchainCost(
		t.Context(), c.db, mockLnd.Client, mockLnd.WalletKit,
		c.depositStore, c.swapStore,
	)
	require.NoError(t, err)
	require.Nil(t, c.onchainCost(unknown.SwapHash))
}

// TestMigrateOnchainCostRetriedSwap tests that the migration uses the deposits
// a swap recorded, even if the deposit to swap mapping points to an earlier
// failed attempt that used the same deposits.
func TestMigrateOnchainCostRetriedSwap(t *testing.T) {
	c := newOnchainCostTestContext(t)

	tx := fundingTx(1, otherOut(10_000), staticOut(100_000))
	mockLnd := test.NewMockLnd()
	mockLnd.Transactions = []lndclient.Transaction{
		walletTx(tx, 700, true),
	}

	failed := c.createLoopIn(
		lntypes.Hash{0x1}, Failed,
		wire.OutPoint{Hash: tx.TxHash(), Index: 1},
	)
	retry := c.createLoopInWithDeposits(
		lntypes.Hash{0x2}, Succeeded, failed.Deposits,
	)

	// Point the deposit back to the failed attempt, which is how
	// MigrateDepositSwapHash maps deposits that were used more than once.
	err := c.swapStore.BatchMapDepositsToSwapHashes(
		t.Context(), map[deposit.ID]lntypes.Hash{
			failed.Deposits[0].ID: failed.SwapHash,
		},
	)
	require.NoError(t, err)

	err = MigrateOnchainCost(
		t.Context(), c.db, mockLnd.Client, mockLnd.WalletKit,
		c.depositStore, c.swapStore,
	)
	require.NoError(t, err)

	require.Equal(t, btcutil.Amount(700), *c.onchainCost(retry.SwapHash))
	require.Nil(t, c.onchainCost(failed.SwapHash))
}

// txSource is a wallet that lists the transactions confirmed in the requested
// blocks and looks them up by hash. It records the listed block ranges and
// counts the lookups of each transaction.
type txSource struct {
	txs     []lndclient.Transaction
	listErr error
	getErr  error

	// listErrStart is the first block of the range that fails to list.
	// If it is zero, every range fails while listErr is set.
	listErrStart int32

	lists [][2]int32
	gets  map[chainhash.Hash]int
}

// newTxSource returns a wallet with the given transactions.
func newTxSource(txs ...lndclient.Transaction) *txSource {
	return &txSource{
		txs:  txs,
		gets: make(map[chainhash.Hash]int),
	}
}

// ListTransactions returns the transactions confirmed between the given
// heights, both included.
func (s *txSource) ListTransactions(_ context.Context, startHeight,
	endHeight int32, _ ...lndclient.ListTransactionsOption) (
	[]lndclient.Transaction, error) {

	s.lists = append(s.lists, [2]int32{startHeight, endHeight})
	if s.listErr != nil &&
		(s.listErrStart == 0 || s.listErrStart == startHeight) {

		return nil, s.listErr
	}

	var listed []lndclient.Transaction
	for _, tx := range s.txs {
		if tx.BlockHeight >= startHeight && tx.BlockHeight <= endHeight {
			listed = append(listed, tx)
		}
	}

	return listed, nil
}

// GetTransaction returns the transaction with the given hash, or the error
// lnd returns if the wallet doesn't have it.
func (s *txSource) GetTransaction(_ context.Context,
	txid chainhash.Hash) (lndclient.Transaction, error) {

	s.gets[txid]++
	if s.getErr != nil {
		return lndclient.Transaction{}, s.getErr
	}

	for _, tx := range s.txs {
		if tx.Tx.TxHash() == txid {
			return tx, nil
		}
	}

	return lndclient.Transaction{}, fmt.Errorf("%w: txid %v",
		wallet.ErrNoTx, txid)
}

// confirmedTx returns the wallet view of a wallet funded transaction that
// confirmed at the given height.
func confirmedTx(tx *wire.MsgTx, fee btcutil.Amount,
	height int32) lndclient.Transaction {

	walletTx := walletTx(tx, fee, true)
	walletTx.BlockHeight = height

	return walletTx
}

// storedDeposit returns a deposit for the given output of a transaction that
// is stored at the given confirmation height.
func storedDeposit(tx *wire.MsgTx, index uint32,
	height int64) *deposit.Deposit {

	return &deposit.Deposit{
		OutPoint:           wire.OutPoint{Hash: tx.TxHash(), Index: index},
		Value:              btcutil.Amount(tx.TxOut[index].Value),
		ConfirmationHeight: height,
	}
}

// TestListFundingTransactions tests that only the blocks with stored deposits
// are listed, in ranges of at most onchainCostListBlocks blocks, and that the
// other funding transactions are looked up once each.
func TestListFundingTransactions(t *testing.T) {
	const (
		height = 100

		// The last block of the range that starts at height.
		edgeHeight = height + onchainCostListBlocks - 1

		// The first block after that range.
		nextHeight = height + onchainCostListBlocks

		// A block after a gap of blocks without deposits.
		farHeight = height + 10*onchainCostListBlocks
	)

	var (
		// Two deposits of one transaction, and deposits at the edge
		// of its range, right after it and after a gap are listed.
		shared = fundingTx(1, staticOut(100_000), staticOut(100_000))
		edge   = fundingTx(2, staticOut(100_000))
		next   = fundingTx(3, staticOut(100_000))
		far    = fundingTx(4, staticOut(100_000))

		// A deposit without a stored height, one with a height that
		// isn't a block height, one whose stored height is outside
		// the listed blocks after a reorg, and one whose transaction
		// isn't in the wallet are looked up.
		noHeight  = fundingTx(5, staticOut(100_000))
		badHeight = fundingTx(6, staticOut(100_000))
		reorged   = fundingTx(7, staticOut(100_000))
		missing   = fundingTx(8, staticOut(100_000))
	)
	source := newTxSource(
		confirmedTx(shared, 100, height),
		confirmedTx(edge, 100, edgeHeight),
		confirmedTx(next, 100, nextHeight),
		confirmedTx(far, 100, farHeight),
		confirmedTx(noHeight, 100, height-50),
		confirmedTx(badHeight, 100, height-50),
		confirmedTx(reorged, 100, height-1),
	)

	deposits := []*deposit.Deposit{
		storedDeposit(shared, 0, height),
		storedDeposit(shared, 1, height),
		storedDeposit(edge, 0, edgeHeight),
		storedDeposit(next, 0, nextHeight),
		storedDeposit(far, 0, farHeight),
		storedDeposit(noHeight, 0, 0),
		storedDeposit(badHeight, 0, math.MaxInt32+1),
		storedDeposit(reorged, 0, height),
		storedDeposit(missing, 0, height),
	}

	txs, err := listFundingTransactions(
		t.Context(), source, source, newStoredDeposits(deposits),
		deposits,
	)
	require.NoError(t, err)

	require.Equal(t, [][2]int32{
		{height, edgeHeight},
		{nextHeight, nextHeight},
		{farHeight, farHeight},
	}, source.lists)
	require.Equal(t, map[chainhash.Hash]int{
		noHeight.TxHash():  1,
		badHeight.TxHash(): 1,
		reorged.TxHash():   1,
		missing.TxHash():   1,
	}, source.gets)

	require.Len(t, txs, 7)
	for _, tx := range []*wire.MsgTx{
		shared, edge, next, far, noHeight, badHeight, reorged,
	} {
		require.Contains(t, txs, tx.TxHash())
	}
}

// TestListFundingTransactionsErrors tests that transactions whose blocks
// can't be listed are looked up one by one, and that a failed lookup is
// returned.
func TestListFundingTransactionsErrors(t *testing.T) {
	tx := fundingTx(1, staticOut(100_000))
	deposits := []*deposit.Deposit{storedDeposit(tx, 0, 100)}

	source := newTxSource(confirmedTx(tx, 100, 100))
	source.listErr = errors.New("lnd unavailable")
	txs, err := listFundingTransactions(
		t.Context(), source, source, newStoredDeposits(deposits),
		deposits,
	)
	require.NoError(t, err)
	require.Contains(t, txs, tx.TxHash())
	require.Equal(t, 1, source.gets[tx.TxHash()])

	source.getErr = errors.New("lnd unavailable")
	_, err = listFundingTransactions(
		t.Context(), source, source, newStoredDeposits(deposits),
		deposits,
	)
	require.ErrorIs(t, err, source.getErr)
}

// TestListFundingTransactionsStopsListing tests that the ranges after a range
// that fails to list aren't listed, and that their transactions are looked up
// one by one instead.
func TestListFundingTransactionsStopsListing(t *testing.T) {
	const (
		firstHeight  = 100
		failedHeight = firstHeight + onchainCostListBlocks
		lastHeight   = firstHeight + 10*onchainCostListBlocks
	)

	first := fundingTx(1, staticOut(100_000))
	failed := fundingTx(2, staticOut(100_000))
	last := fundingTx(3, staticOut(100_000))
	deposits := []*deposit.Deposit{
		storedDeposit(first, 0, firstHeight),
		storedDeposit(failed, 0, failedHeight),
		storedDeposit(last, 0, lastHeight),
	}

	source := newTxSource(
		confirmedTx(first, 100, firstHeight),
		confirmedTx(failed, 100, failedHeight),
		confirmedTx(last, 100, lastHeight),
	)
	source.listErr = errors.New("lnd timed out")
	source.listErrStart = failedHeight

	txs, err := listFundingTransactions(
		t.Context(), source, source, newStoredDeposits(deposits),
		deposits,
	)
	require.NoError(t, err)

	require.Equal(t, [][2]int32{
		{firstHeight, firstHeight},
		{failedHeight, failedHeight},
	}, source.lists)
	require.Equal(t, map[chainhash.Hash]int{
		failed.TxHash(): 1,
		last.TxHash():   1,
	}, source.gets)

	require.Len(t, txs, 3)
	for _, tx := range []*wire.MsgTx{first, failed, last} {
		require.Contains(t, txs, tx.TxHash())
	}
}

// TestMigrateOnchainCostRetriesFailures tests that a failed wallet lookup
// keeps the migration from being marked as done, so that it is retried on the
// next start.
func TestMigrateOnchainCostRetriesFailures(t *testing.T) {
	c := newOnchainCostTestContext(t)

	tx := fundingTx(1, otherOut(10_000), staticOut(100_000))
	source := newTxSource(confirmedTx(tx, 700, 100))

	swap := c.createLoopIn(
		lntypes.Hash{0x1}, Succeeded,
		wire.OutPoint{Hash: tx.TxHash(), Index: 1},
	)

	// The first run can't list or look up the wallet transactions.
	source.listErr = errors.New("lnd unavailable")
	source.getErr = source.listErr
	err := MigrateOnchainCost(
		t.Context(), c.db, source, source, c.depositStore, c.swapStore,
	)
	require.ErrorIs(t, err, source.getErr)
	require.Nil(t, c.onchainCost(swap.SwapHash))

	done, err := c.db.HasMigration(t.Context(), onchainCostMigrationID)
	require.NoError(t, err)
	require.False(t, done)

	// The next run succeeds and marks the migration as done.
	source.listErr = nil
	source.getErr = nil
	err = MigrateOnchainCost(
		t.Context(), c.db, source, source, c.depositStore, c.swapStore,
	)
	require.NoError(t, err)
	require.Equal(t, btcutil.Amount(700), *c.onchainCost(swap.SwapHash))

	done, err = c.db.HasMigration(t.Context(), onchainCostMigrationID)
	require.NoError(t, err)
	require.True(t, done)
}

// TestMigrateOnchainCostSkipsInvalidSwap tests that a swap whose cost can't
// be determined from its deposits keeps an unknown cost without keeping the
// migration from being marked as done, since the error would repeat on every
// start.
func TestMigrateOnchainCostSkipsInvalidSwap(t *testing.T) {
	c := newOnchainCostTestContext(t)

	tx := fundingTx(1, otherOut(10_000), staticOut(100_000))
	source := newTxSource(confirmedTx(tx, 700, 100))

	valid := c.createLoopIn(
		lntypes.Hash{0x1}, Succeeded,
		wire.OutPoint{Hash: tx.TxHash(), Index: 1},
	)

	// The deposit points to an output the funding transaction doesn't
	// have, so its fee share can't be computed.
	invalid := c.createLoopIn(
		lntypes.Hash{0x2}, Succeeded,
		wire.OutPoint{Hash: tx.TxHash(), Index: 5},
	)

	err := MigrateOnchainCost(
		t.Context(), c.db, source, source, c.depositStore, c.swapStore,
	)
	require.NoError(t, err)
	require.Equal(t, btcutil.Amount(700), *c.onchainCost(valid.SwapHash))
	require.Nil(t, c.onchainCost(invalid.SwapHash))

	done, err := c.db.HasMigration(t.Context(), onchainCostMigrationID)
	require.NoError(t, err)
	require.True(t, done)
}
