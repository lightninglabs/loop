package deposit

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"math/big"
	"strings"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/wallet"
	"github.com/lightninglabs/lndclient"
)

// walletTxGetter looks up transactions of the client wallet.
type walletTxGetter interface {
	// GetTransaction returns the wallet transaction with the given hash.
	GetTransaction(ctx context.Context,
		txid chainhash.Hash) (lndclient.Transaction, error)
}

// lndTxNotFound is the error message lnd returns for a transaction that isn't
// in its wallet. lnd returns it without a gRPC status code, so only its message
// can be matched. If the message ever changes, such a deposit is retried
// instead of being marked unknown.
var lndTxNotFound = wallet.ErrNoTx.Error()

// depositsByFundingTx groups deposits by funding transaction, in the order
// in which the transactions first appear.
func depositsByFundingTx(deposits []*Deposit) [][]*Deposit {
	var (
		groups [][]*Deposit
		index  = make(map[chainhash.Hash]int)
	)
	for _, d := range deposits {
		i, ok := index[d.Hash]
		if !ok {
			i = len(groups)
			index[d.Hash] = i
			groups = append(groups, nil)
		}
		groups[i] = append(groups[i], d)
	}

	return groups
}

// onchainCosts determines the on-chain cost of the given deposits from the
// fees of their funding transactions, which are looked up in the client
// wallet once each. It returns the costs by outpoint, so the deposits only
// need their outpoint set. isDeposit reports whether an outpoint is a known
// deposit.
//
// It stops at the first funding transaction it can't look up, for example
// because lnd is unavailable or loopd is shutting down, and returns the costs
// found until then along with an error that names that transaction. The
// deposits it didn't determine are left out, so that they can be looked up
// again later.
func onchainCosts(ctx context.Context, getter walletTxGetter,
	isDeposit func(wire.OutPoint) bool,
	deposits []*Deposit) (map[wire.OutPoint]OnchainCost, error) {

	costs := make(map[wire.OutPoint]OnchainCost, len(deposits))
	for _, txDeposits := range depositsByFundingTx(deposits) {
		txCosts, err := fundingTxCosts(
			ctx, getter, isDeposit, txDeposits,
		)
		if err != nil {
			return costs, err
		}

		maps.Copy(costs, txCosts)
	}

	return costs, nil
}

// fundingTxCosts looks up the funding transaction of the given deposits, which
// all have the same one, and returns their on-chain costs.
func fundingTxCosts(ctx context.Context, getter walletTxGetter,
	isDeposit func(wire.OutPoint) bool,
	deposits []*Deposit) (map[wire.OutPoint]OnchainCost, error) {

	txid := deposits[0].Hash
	tx, err := getter.GetTransaction(ctx, txid)
	switch {
	// If the wallet doesn't have the funding transaction, for example
	// after a restore that lost its history, the fee is unknown.
	case err != nil && strings.Contains(err.Error(), lndTxNotFound):
		costs := make(map[wire.OutPoint]OnchainCost, len(deposits))
		setCosts(costs, deposits, OnchainCost{
			Status: OnchainCostUnknown,
		})

		return costs, nil

	case err != nil:
		return nil, fmt.Errorf("funding tx %v: %w", txid, err)
	}

	return txOnchainCosts(tx, isDeposit, deposits), nil
}

// txOnchainCosts returns the on-chain cost of the given deposits, which were
// all created by the given wallet transaction.
func txOnchainCosts(tx lndclient.Transaction,
	isDeposit func(wire.OutPoint) bool,
	deposits []*Deposit) map[wire.OutPoint]OnchainCost {

	costs := make(map[wire.OutPoint]OnchainCost, len(deposits))
	unknown := OnchainCost{Status: OnchainCostUnknown}
	if tx.Tx == nil {
		setCosts(costs, deposits, unknown)

		return costs
	}

	// A deposit created by a transaction that spent earlier deposits, like
	// the change of a partial loop-in or withdrawal, brought in no new
	// funds. Its fee belongs to that earlier swap or withdrawal, so the
	// deposit costs nothing.
	if spendsDeposits(tx.Tx, isDeposit) {
		setCosts(costs, deposits, OnchainCost{
			Status: OnchainCostKnown,
		})

		return costs
	}

	// If an input isn't from the client wallet, for example for a deposit
	// sent from an exchange, the wallet doesn't know the fee.
	if !allInputsOurs(tx) {
		setCosts(costs, deposits, unknown)

		return costs
	}

	outputs := fundingTxDeposits(tx.Tx, isDeposit)
	for _, d := range deposits {
		share, err := depositFeeShare(tx.Tx, tx.Fee, d.Index, outputs)
		if err != nil {
			// The transaction doesn't match the deposit, so asking
			// again won't help.
			log.Warnf("Unable to determine the on-chain cost of "+
				"deposit %v, treating it as unknown: %v",
				d.OutPoint, err)

			costs[d.OutPoint] = unknown

			continue
		}

		costs[d.OutPoint] = OnchainCost{
			Amount: share,
			Status: OnchainCostKnown,
		}
	}

	return costs
}

// setCosts sets the given cost for all of the given deposits.
func setCosts(costs map[wire.OutPoint]OnchainCost, deposits []*Deposit,
	cost OnchainCost) {

	for _, d := range deposits {
		costs[d.OutPoint] = cost
	}
}

// fundingTxDeposits returns the indexes of the outputs of a funding
// transaction that are known deposits, so that deposits to different static
// addresses are all included.
func fundingTxDeposits(tx *wire.MsgTx,
	isDeposit func(wire.OutPoint) bool) map[uint32]struct{} {

	txHash := tx.TxHash()
	indexes := make(map[uint32]struct{})
	for i := range tx.TxOut {
		if isDeposit(wire.OutPoint{Hash: txHash, Index: uint32(i)}) {
			indexes[uint32(i)] = struct{}{}
		}
	}

	return indexes
}

// spendsDeposits returns true if an input of the transaction spends a known
// deposit.
func spendsDeposits(tx *wire.MsgTx, isDeposit func(wire.OutPoint) bool) bool {
	for _, txIn := range tx.TxIn {
		if isDeposit(txIn.PreviousOutPoint) {
			return true
		}
	}

	return false
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
