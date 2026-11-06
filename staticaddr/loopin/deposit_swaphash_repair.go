package loopin

import (
	"bytes"
	"context"
	"fmt"
	"slices"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightningnetwork/lnd/lntypes"
)

const (
	// htlcTimeoutRepairMigrationID is the identifier for the migration
	// that repairs the swap hashes of deposits on the htlc timeout path.
	htlcTimeoutRepairMigrationID = "deposit_swap_hash_repair_htlc_timeout"
)

// WalletTxLister lists the transactions of the client wallet.
type WalletTxLister interface {
	// ListTransactions returns the wallet transactions between the given
	// heights. An end height of -1 includes unconfirmed transactions.
	ListTransactions(ctx context.Context, startHeight, endHeight int32,
		opts ...lndclient.ListTransactionsOption) ([]lndclient.Transaction,
		error)
}

// MigrateRepairHtlcTimeoutDepositSwapHash maps each deposit on the htlc
// timeout path to the swap whose htlc tx spent it: a swap on its own htlc
// timeout path whose htlc tx is the confirmed wallet tx that spent the
// deposit. A deposit is left as it is if no swap qualifies. If lnd can't list
// the wallet txs, the repair is skipped and runs again at the next start.
func MigrateRepairHtlcTimeoutDepositSwapHash(ctx context.Context,
	db loopdb.SwapStore, depositStore *deposit.SqlStore,
	swapStore *SqlStore, wallet WalletTxLister) error {

	done, err := db.HasMigration(ctx, htlcTimeoutRepairMigrationID)
	if err != nil {
		return fmt.Errorf("unable to check migration status: %w", err)
	}
	if done {
		return nil
	}

	log.Infof("Repairing the swap hashes of deposits on the htlc timeout " +
		"path")

	allDeposits, err := depositStore.AllDeposits(ctx)
	if err != nil {
		return err
	}

	// Only deposits on the htlc timeout path are repaired. Their htlc tx
	// can't confirm before them, so list wallet txs from the first of them.
	var (
		deposits    []*deposit.Deposit
		startHeight int64
	)
	for _, d := range allDeposits {
		if !slices.Contains(htlcTimeoutDepositStates, d.GetState()) {
			continue
		}

		if len(deposits) == 0 || d.ConfirmationHeight < startHeight {
			startHeight = d.ConfirmationHeight
		}
		deposits = append(deposits, d)
	}

	if len(deposits) == 0 {
		return db.SetMigration(ctx, htlcTimeoutRepairMigrationID)
	}

	// Find the wallet tx that spent each outpoint. Don't keep loopd from
	// starting if lnd can't list them.
	txs, err := wallet.ListTransactions(ctx, int32(startHeight), -1)
	if err != nil {
		log.Warnf("Unable to list wallet transactions, the deposit "+
			"swap hash repair runs again at the next start: %v",
			err)

		return nil
	}

	// A deposit on the htlc timeout path was spent by a confirmed tx, so
	// unconfirmed txs, which might conflict with it, are skipped.
	spendingTxs := make(map[wire.OutPoint]*wire.MsgTx)
	for _, tx := range txs {
		if tx.Tx == nil || tx.Confirmations <= 0 {
			continue
		}

		for _, txIn := range tx.Tx.TxIn {
			spendingTxs[txIn.PreviousOutPoint] = tx.Tx
		}
	}

	swapsForOutpoint, err := swapsBySelectedOutpoint(ctx, swapStore)
	if err != nil {
		return err
	}

	depositsToSwapHashes := make(map[deposit.ID]lntypes.Hash)
	for _, d := range deposits {
		// Find the swap on its htlc timeout path whose htlc tx spent
		// the deposit. Each swap has its own htlc, so only one can
		// qualify.
		spendingTx := spendingTxs[d.OutPoint]

		var (
			spender lntypes.Hash
			found   bool
		)
		for _, swap := range swapsForOutpoint[d.OutPoint] {
			if onOwnHtlcTimeoutPath(swap) &&
				isHtlcTx(swap, spendingTx, swapStore.network) {

				spender, found = swap.SwapHash, true

				break
			}
		}

		if !found || (d.SwapHash != nil && *d.SwapHash == spender) {
			continue
		}

		log.Infof("Mapping deposit %v to swap %v, which spent it, "+
			"instead of swap %v", d.OutPoint, spender, d.SwapHash)

		depositsToSwapHashes[d.ID] = spender
	}

	return applyDepositSwapHashRepair(
		ctx, db, swapStore, htlcTimeoutRepairMigrationID,
		depositsToSwapHashes,
	)
}

// isHtlcTx returns true if the tx is the swap's htlc tx: it spends exactly the
// deposits the swap signed its htlc tx for and pays to the swap's htlc. The
// inputs alone aren't enough, since two swaps can select the same deposits.
func isHtlcTx(swap *StaticAddressLoopIn, tx *wire.MsgTx,
	chainParams *chaincfg.Params) bool {

	outpoints, err := swap.signedOutpoints()
	if err != nil {
		log.Warnf("Unable to determine the deposits of swap %v: %v",
			swap.SwapHash, err)

		return false
	}

	if !spendsExactly(tx, outpoints) {
		return false
	}

	htlc, err := swap.getHtlc(chainParams)
	if err != nil {
		log.Warnf("Unable to determine the htlc of swap %v: %v",
			swap.SwapHash, err)

		return false
	}

	return slices.ContainsFunc(tx.TxOut, func(txOut *wire.TxOut) bool {
		return bytes.Equal(txOut.PkScript, htlc.PkScript)
	})
}

// swapsBySelectedOutpoint returns all swaps by the deposit outpoints they
// selected.
func swapsBySelectedOutpoint(ctx context.Context, swapStore *SqlStore) (
	map[wire.OutPoint][]*StaticAddressLoopIn, error) {

	swaps, err := swapStore.GetStaticAddressLoopInSwapsByStates(
		ctx, AllStates,
	)
	if err != nil {
		return nil, err
	}

	swapsForOutpoint := make(map[wire.OutPoint][]*StaticAddressLoopIn)
	for _, swap := range swaps {
		outpoints, err := parseOutpoints(swap.DepositOutpoints)
		if err != nil {
			log.Warnf("Unable to parse the deposits of swap %v: %v",
				swap.SwapHash, err)

			continue
		}

		for _, outpoint := range outpoints {
			swapsForOutpoint[outpoint] = append(
				swapsForOutpoint[outpoint], swap,
			)
		}
	}

	return swapsForOutpoint, nil
}

// applyDepositSwapHashRepair maps the deposits to the given swap hashes and
// marks the repair migration with the given id as done.
func applyDepositSwapHashRepair(ctx context.Context, db loopdb.SwapStore,
	swapStore *SqlStore, migrationID string,
	depositsToSwapHashes map[deposit.ID]lntypes.Hash) error {

	log.Infof("Repairing the swap hash of %d deposits",
		len(depositsToSwapHashes))

	err := swapStore.BatchMapDepositsToSwapHashes(ctx, depositsToSwapHashes)
	if err != nil {
		return err
	}

	return db.SetMigration(ctx, migrationID)
}
