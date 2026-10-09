package loopin

import (
	"bytes"
	"context"
	"fmt"
	"math/big"
	"strings"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/wallet"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/staticaddr/deposit"
)

// walletTxGetter looks up transactions of the client wallet.
type walletTxGetter interface {
	// GetTransaction returns the wallet transaction with the given hash.
	GetTransaction(ctx context.Context,
		txid chainhash.Hash) (lndclient.Transaction, error)
}

// lndTxNotFound is the error message lnd returns for a transaction that isn't
// in its wallet. The error loses its type over gRPC, so only its message can
// be matched.
var lndTxNotFound = wallet.ErrNoTx.Error()

// depositLookup finds the known deposits behind outpoints.
type depositLookup interface {
	// DepositsForOutpoints returns the deposits behind the given
	// outpoints. If ignoreUnknown is true, unknown outpoints are skipped.
	DepositsForOutpoints(ctx context.Context, outpoints []string,
		ignoreUnknown bool) ([]*deposit.Deposit, error)
}

// depositOnchainCost returns the on-chain cost of the given deposits, which is
// the sum of their funding fee shares. The second return value is false if
// the fee of a funding transaction is unknown.
func depositOnchainCost(ctx context.Context, getter walletTxGetter,
	lookup depositLookup,
	deposits []*deposit.Deposit) (btcutil.Amount, bool, error) {

	txs, err := fundingTransactions(ctx, getter, deposits)
	if err != nil {
		return 0, false, err
	}

	return onchainCostFromTxs(ctx, lookup, txs, deposits)
}

// fundingTransactions looks up the transactions that funded the given
// deposits in the client wallet, keyed by transaction hash. Transactions that
// aren't in the wallet are left out, so their fee is unknown.
func fundingTransactions(ctx context.Context, getter walletTxGetter,
	deposits []*deposit.Deposit) (map[chainhash.Hash]lndclient.Transaction,
	error) {

	txs := make(map[chainhash.Hash]lndclient.Transaction)
	looked := make(map[chainhash.Hash]struct{})
	for _, d := range deposits {
		if _, ok := looked[d.Hash]; ok {
			continue
		}
		looked[d.Hash] = struct{}{}

		tx, err := getter.GetTransaction(ctx, d.Hash)
		switch {
		case err != nil && strings.Contains(err.Error(), lndTxNotFound):
			continue

		case err != nil:
			return nil, fmt.Errorf("unable to look up funding tx %v: "+
				"%w", d.Hash, err)
		}

		txs[d.Hash] = tx
	}

	return txs, nil
}

// onchainCostFromTxs returns the on-chain cost of the given deposits using
// the given funding transactions. The second return value is false if the fee
// of a funding transaction is unknown.
func onchainCostFromTxs(ctx context.Context, lookup depositLookup,
	txsByHash map[chainhash.Hash]lndclient.Transaction,
	deposits []*deposit.Deposit) (btcutil.Amount, bool, error) {

	if len(deposits) == 0 {
		return 0, false, fmt.Errorf("no deposits")
	}

	// The deposit outputs of each funding transaction and whether it spends
	// earlier deposits, so that we only look them up once per transaction.
	var (
		depositOutputs = make(map[chainhash.Hash]map[uint32]struct{})
		spendsDeps     = make(map[chainhash.Hash]bool)
	)

	var total btcutil.Amount
	for _, d := range deposits {
		tx, ok := txsByHash[d.Hash]
		if !ok || tx.Tx == nil {
			return 0, false, nil
		}

		spends, ok := spendsDeps[d.Hash]
		if !ok {
			var err error
			spends, err = spendsDeposits(ctx, lookup, tx.Tx)
			if err != nil {
				return 0, false, err
			}

			spendsDeps[d.Hash] = spends
		}

		// A deposit created by a transaction that spent earlier
		// deposits, like the change of a partial loop-in or withdrawal,
		// brought in no new funds. Its fee belongs to that earlier swap
		// or withdrawal, so the deposit costs nothing.
		if spends {
			continue
		}

		if !allInputsOurs(tx) {
			return 0, false, nil
		}

		outputs, ok := depositOutputs[d.Hash]
		if !ok {
			var err error
			outputs, err = fundingTxDeposits(ctx, lookup, tx.Tx)
			if err != nil {
				return 0, false, err
			}

			depositOutputs[d.Hash] = outputs
		}

		share, err := depositFeeShare(tx.Tx, tx.Fee, d.Index, outputs)
		if err != nil {
			return 0, false, fmt.Errorf("deposit %v: %w", d.OutPoint,
				err)
		}

		total += share
	}

	return total, true, nil
}

// fundingTxDeposits returns the indexes of the outputs of a funding
// transaction that are known deposits. Deposits are looked up by outpoint, so
// that deposits to different static addresses are all included.
func fundingTxDeposits(ctx context.Context, lookup depositLookup,
	tx *wire.MsgTx) (map[uint32]struct{}, error) {

	txHash := tx.TxHash()
	outpoints := make([]string, len(tx.TxOut))
	for i := range tx.TxOut {
		outpoints[i] = wire.NewOutPoint(&txHash, uint32(i)).String()
	}

	deposits, err := lookup.DepositsForOutpoints(ctx, outpoints, true)
	if err != nil {
		return nil, fmt.Errorf("unable to look up deposits of funding "+
			"tx %v: %w", txHash, err)
	}

	indexes := make(map[uint32]struct{}, len(deposits))
	for _, d := range deposits {
		indexes[d.Index] = struct{}{}
	}

	return indexes, nil
}

// spendsDeposits returns true if an input of the transaction spends a known
// deposit.
func spendsDeposits(ctx context.Context, lookup depositLookup,
	tx *wire.MsgTx) (bool, error) {

	outpoints := make([]string, len(tx.TxIn))
	for i, txIn := range tx.TxIn {
		outpoints[i] = txIn.PreviousOutPoint.String()
	}

	deposits, err := lookup.DepositsForOutpoints(ctx, outpoints, true)
	if err != nil {
		return false, fmt.Errorf("unable to look up deposits spent by "+
			"tx %v: %w", tx.TxHash(), err)
	}

	return len(deposits) > 0, nil
}

// allInputsOurs returns true if all inputs of the transaction belong to the
// client wallet. Only then does the wallet know the transaction fee.
func allInputsOurs(tx lndclient.Transaction) bool {
	if len(tx.PreviousOutpoints) == 0 {
		return false
	}

	for _, prevOut := range tx.PreviousOutpoints {
		if !prevOut.IsOurOutput {
			return false
		}
	}

	return true
}

// depositFeeShare returns the part of the funding transaction fee charged to
// the deposit at the given output index. The fee is split by value across the
// deposit outputs, and the sats left over by integer division go to the
// deposit output with the lowest index.
func depositFeeShare(tx *wire.MsgTx, fee btcutil.Amount, index uint32,
	depositOutputs map[uint32]struct{}) (btcutil.Amount, error) {

	if int(index) >= len(tx.TxOut) {
		return 0, fmt.Errorf("output index %d out of range", index)
	}

	if fee < 0 {
		return 0, fmt.Errorf("negative fee %v", fee)
	}

	// Besides the known deposits, outputs to the deposit's own address are
	// deposits too, so a sibling deposit that isn't stored yet still gets
	// its share.
	pkScript := tx.TxOut[index].PkScript
	isDeposit := func(i int) bool {
		_, known := depositOutputs[uint32(i)]

		return known || bytes.Equal(tx.TxOut[i].PkScript, pkScript)
	}

	var (
		lowestIndex = -1
		totalValue  int64
	)
	for i, txOut := range tx.TxOut {
		if !isDeposit(i) {
			continue
		}

		if lowestIndex == -1 {
			lowestIndex = i
		}
		totalValue += txOut.Value
	}

	if totalValue <= 0 {
		return 0, fmt.Errorf("deposit outputs have no value")
	}

	// Use big integers so that fee * value can't overflow.
	shareOf := func(value int64) int64 {
		share := new(big.Int).Mul(
			big.NewInt(int64(fee)), big.NewInt(value),
		)
		share.Quo(share, big.NewInt(totalValue))

		return share.Int64()
	}

	share := shareOf(tx.TxOut[index].Value)
	if int(index) != lowestIndex {
		return btcutil.Amount(share), nil
	}

	// The lowest index deposit output also gets the remainder.
	var allocated int64
	for i, txOut := range tx.TxOut {
		if !isDeposit(i) {
			continue
		}

		allocated += shareOf(txOut.Value)
	}

	return btcutil.Amount(share + int64(fee) - allocated), nil
}

// outpointDeposits returns deposits for the given outpoints. Only their
// outpoints are set, which is all that the on-chain cost needs.
func outpointDeposits(outpoints []string) ([]*deposit.Deposit, error) {
	deposits := make([]*deposit.Deposit, 0, len(outpoints))
	for _, o := range outpoints {
		outpoint, err := wire.NewOutPointFromString(o)
		if err != nil {
			return nil, err
		}

		deposits = append(deposits, &deposit.Deposit{
			OutPoint: *outpoint,
		})
	}

	return deposits, nil
}
