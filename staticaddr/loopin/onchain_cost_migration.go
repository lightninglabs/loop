package loopin

import (
	"context"
	"fmt"
	"maps"
	"math"
	"slices"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightningnetwork/lnd/lntypes"
)

const (
	// onchainCostMigrationID is the identifier for the static loop-in
	// on-chain cost migration.
	onchainCostMigrationID = "static_loopin_onchain_cost"

	// onchainCostListBlocks is the number of blocks whose wallet
	// transactions the migration lists at once, which bounds the size of
	// each response for busy wallets.
	onchainCostListBlocks = 4032

	// onchainCostProgressMin is the number of swaps from which the
	// migration logs its progress.
	onchainCostProgressMin = 100
)

// walletTxLister lists the transactions of the client wallet.
type walletTxLister interface {
	// ListTransactions returns the wallet transactions confirmed between
	// the given heights, both included.
	ListTransactions(ctx context.Context, startHeight, endHeight int32,
		opts ...lndclient.ListTransactionsOption) ([]lndclient.Transaction,
		error)
}

// depositLister returns all stored deposits.
type depositLister interface {
	// AllDeposits returns all stored deposits.
	AllDeposits(ctx context.Context) ([]*deposit.Deposit, error)
}

// MigrateOnchainCost sets the on-chain cost of past static address loop-ins
// that consumed their deposits before the cost was recorded. Swaps whose
// funding transaction fees are unknown to the wallet, or whose cost can't be
// determined from the stored deposits, keep an unknown cost. If loading the
// swaps, the deposits or the wallet transactions, or storing the costs fails,
// the migration returns an error without being marked as done, so it is
// retried on the next start.
//
// The migration runs on startup and can have many swaps, so it loads all
// deposits at once and lists the wallet transactions in ranges of blocks
// instead of looking up each of them. It only sets the cost of swaps that
// don't have one yet.
func MigrateOnchainCost(ctx context.Context, db loopdb.SwapStore,
	lister walletTxLister, getter walletTxGetter, depositStore depositLister,
	swapStore *SqlStore) error {

	migrationDone, err := db.HasMigration(ctx, onchainCostMigrationID)
	if err != nil {
		return fmt.Errorf("unable to check migration status: %w", err)
	}
	if migrationDone {
		log.Infof("Static loop-in on-chain cost migration already " +
			"done, skipping")

		return nil
	}

	log.Infof("Starting static loop-in on-chain cost migration")
	startTs := time.Now()

	swaps, err := swapStore.swapsWithoutOnchainCost(ctx)
	if err != nil {
		return err
	}

	// Collect the swaps that consumed their deposits. Their deposits are
	// taken from the outpoints that each swap recorded, since the deposit
	// to swap mapping of past swaps can point to an earlier failed
	// attempt.
	var (
		pending      []swapWithoutOnchainCost
		swapDeposits = make(map[lntypes.Hash][]*deposit.Deposit)
		deposits     []*deposit.Deposit
	)
	for _, swap := range swaps {
		if !slices.Contains(ConsumedStates, swap.State) ||
			len(swap.DepositOutpoints) == 0 {

			continue
		}

		// Outpoints that can't be parsed won't parse on a retry
		// either, so the swap keeps an unknown cost.
		swapDeps, err := outpointDeposits(swap.DepositOutpoints)
		if err != nil {
			log.Warnf("Unable to parse deposits of static loop-in "+
				"%v: %v", swap.SwapHash, err)

			continue
		}

		pending = append(pending, swap)
		swapDeposits[swap.SwapHash] = swapDeps
		deposits = append(deposits, swapDeps...)
	}

	if len(pending) == 0 {
		return finishOnchainCostMigration(ctx, db, startTs)
	}

	log.Infof("Backfilling the on-chain cost of %d past static loop-ins",
		len(pending))

	allDeposits, err := depositStore.AllDeposits(ctx)
	if err != nil {
		return fmt.Errorf("unable to load deposits: %w", err)
	}
	lookup := newStoredDeposits(allDeposits)

	txs, err := listFundingTransactions(
		ctx, lister, getter, lookup, deposits,
	)
	if err != nil {
		return err
	}

	var (
		onchainCosts = make(map[lntypes.Hash]btcutil.Amount)
		logStep      int
	)
	if len(pending) >= onchainCostProgressMin {
		logStep = len(pending) / 10
	}
	for i, swap := range pending {
		cost, known, err := onchainCostFromTxs(
			ctx, lookup, txs, swapDeposits[swap.SwapHash],
		)
		switch {
		// The deposits and transactions are loaded already, so the
		// error would repeat on a retry and the swap keeps an unknown
		// cost.
		case err != nil:
			log.Warnf("Unable to determine on-chain cost of static "+
				"loop-in %v, keeping it unknown: %v",
				swap.SwapHash, err)

		case known:
			onchainCosts[swap.SwapHash] = cost
		}

		if logStep > 0 && (i+1)%logStep == 0 {
			log.Infof("Static loop-in on-chain cost migration: "+
				"%d/%d swaps", i+1, len(pending))
		}
	}

	log.Infof("Setting the on-chain cost of %d of %d static loop-ins",
		len(onchainCosts), len(pending))

	err = swapStore.BatchSetUnknownOnchainCosts(ctx, onchainCosts)
	if err != nil {
		return err
	}

	return finishOnchainCostMigration(ctx, db, startTs)
}

// finishOnchainCostMigration marks the on-chain cost migration as done and
// logs how long it took.
func finishOnchainCostMigration(ctx context.Context, db loopdb.SwapStore,
	startTs time.Time) error {

	err := db.SetMigration(ctx, onchainCostMigrationID)
	if err != nil {
		return err
	}

	log.Infof("Finished static loop-in on-chain cost migration in %v",
		time.Since(startTs))

	return nil
}

// listFundingTransactions returns the wallet transactions that funded the
// given deposits, keyed by transaction hash. It lists the blocks returned by
// blockRanges and looks up the transactions that weren't listed one by one,
// for example after a reorg. If listing fails, it stops listing, since lnd
// might not respond until a timeout, and looks up the rest one by one.
// Transactions that aren't in the wallet are left out, so their fee is
// unknown.
func listFundingTransactions(ctx context.Context, lister walletTxLister,
	getter walletTxGetter, stored storedDeposits,
	deposits []*deposit.Deposit) (map[chainhash.Hash]lndclient.Transaction,
	error) {

	needed := make(map[chainhash.Hash]struct{})
	for _, d := range deposits {
		needed[d.Hash] = struct{}{}
	}

	txs := make(map[chainhash.Hash]lndclient.Transaction)
	ranges := blockRanges(stored, deposits)
	for i, r := range ranges {
		listed, err := lister.ListTransactions(ctx, r[0], r[1])
		if err != nil {
			log.Warnf("Unable to list wallet transactions of blocks "+
				"%d to %d, looking up the remaining transactions "+
				"one by one: %v", r[0], r[1], err)

			break
		}

		for _, tx := range listed {
			if tx.Tx == nil {
				continue
			}

			hash := tx.Tx.TxHash()
			if _, ok := needed[hash]; ok {
				txs[hash] = tx
			}
		}

		log.Infof("Listed wallet transactions of blocks %d to %d "+
			"(%d/%d)", r[0], r[1], i+1, len(ranges))
	}

	var unlisted []*deposit.Deposit
	for _, d := range deposits {
		if _, ok := txs[d.Hash]; !ok {
			unlisted = append(unlisted, d)
		}
	}

	unlistedTxs, err := fundingTransactions(ctx, getter, unlisted)
	if err != nil {
		return nil, err
	}
	maps.Copy(txs, unlistedTxs)

	return txs, nil
}

// blockRanges returns the ranges of blocks, both ends included, to list for
// the given deposits. Each range starts at the confirmation height of a stored
// deposit that no earlier range covers, has at most onchainCostListBlocks
// blocks and ends at the last deposit height it covers. Deposits that aren't
// stored or whose height isn't a valid block height aren't covered.
func blockRanges(stored storedDeposits,
	deposits []*deposit.Deposit) [][2]int32 {

	var heights []int32
	for _, d := range deposits {
		storedDeposit, ok := stored[d.OutPoint]
		if !ok || storedDeposit.ConfirmationHeight <= 0 ||
			storedDeposit.ConfirmationHeight > math.MaxInt32 {

			continue
		}

		heights = append(heights, int32(storedDeposit.ConfirmationHeight))
	}
	slices.Sort(heights)
	heights = slices.Compact(heights)

	var ranges [][2]int32
	for i := 0; i < len(heights); {
		start := heights[i]
		last := int64(start) + onchainCostListBlocks - 1

		end := start
		for i < len(heights) && int64(heights[i]) <= last {
			end = heights[i]
			i++
		}

		ranges = append(ranges, [2]int32{start, end})
	}

	return ranges
}

// storedDeposits looks up deposits in a set of deposits that were loaded at
// once, keyed by outpoint.
type storedDeposits map[wire.OutPoint]*deposit.Deposit

// newStoredDeposits returns a lookup for the given deposits.
func newStoredDeposits(deposits []*deposit.Deposit) storedDeposits {
	stored := make(storedDeposits, len(deposits))
	for _, d := range deposits {
		stored[d.OutPoint] = d
	}

	return stored
}

// DepositsForOutpoints returns the deposits behind the given outpoints. If
// ignoreUnknown is true, unknown outpoints are skipped.
func (s storedDeposits) DepositsForOutpoints(_ context.Context,
	outpoints []string, ignoreUnknown bool) ([]*deposit.Deposit, error) {

	deposits := make([]*deposit.Deposit, 0, len(outpoints))
	seen := make(map[wire.OutPoint]struct{}, len(outpoints))
	for i, o := range outpoints {
		op, err := wire.NewOutPointFromString(o)
		if err != nil {
			return nil, err
		}

		if _, ok := seen[*op]; ok {
			return nil, fmt.Errorf("duplicate outpoint %s at index "+
				"%d", o, i)
		}
		seen[*op] = struct{}{}

		d, ok := s[*op]
		if !ok {
			if ignoreUnknown {
				continue
			}

			return nil, fmt.Errorf("%w: %v", deposit.ErrDepositNotFound,
				o)
		}

		deposits = append(deposits, d)
	}

	return deposits, nil
}
