package loopin

import (
	"bytes"
	"context"
	"fmt"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
)

// lndTxOutChecker checks outpoint availability using lnd's wallet transaction
// view. It omits outputs already spent by a wallet-known transaction.
type lndTxOutChecker struct {
	client lndclient.LightningClient
}

// NewLndTxOutChecker creates a TxOutChecker backed by lnd.
func NewLndTxOutChecker(client lndclient.LightningClient) TxOutChecker {
	return &lndTxOutChecker{
		client: client,
	}
}

// GetTxOuts returns all requested tx outputs that lnd's transaction view still
// reports as unspent.
func (c *lndTxOutChecker) GetTxOuts(ctx context.Context,
	outpoints []wire.OutPoint) (map[wire.OutPoint]*wire.TxOut, error) {

	outputs, _, err := c.CheckDepositSpend(ctx, outpoints, nil, nil)
	return outputs, err
}

// CheckDepositSpend checks one wallet snapshot for available originals and a
// confirmed spend that makes this swap's HTLC impossible on the current chain.
// The retained funding transactions also prove conflicts with funding parents
// that lnd has since forgotten. Missing history and unconfirmed conflicts never
// establish that proof.
func (c *lndTxOutChecker) CheckDepositSpend(ctx context.Context,
	outpoints []wire.OutPoint, htlcPkScript []byte, fundingTxs []*wire.MsgTx) (
	map[wire.OutPoint]*wire.TxOut, bool, error) {

	outpointByString := make(map[string]wire.OutPoint, len(outpoints))
	outpointsByHash := make(map[string][]wire.OutPoint, len(outpoints))
	for _, outpoint := range outpoints {
		outpointByString[outpoint.String()] = outpoint
		outpointsByHash[outpoint.Hash.String()] = append(
			outpointsByHash[outpoint.Hash.String()], outpoint,
		)
	}

	// Only ancestors whose transaction hash actually funds an original
	// deposit can establish a conflict. Sharing an input with a different
	// sufficiently confirmed transaction proves that parent cannot confirm.
	fundingInputs := make(map[wire.OutPoint][]chainhash.Hash)
	for _, fundingTx := range fundingTxs {
		if err := validateFundingTransaction(fundingTx); err != nil {
			return nil, false, err
		}
		hash := fundingTx.TxHash()
		originals := outpointsByHash[hash.String()]
		if len(originals) == 0 {
			return nil, false, fmt.Errorf("funding snapshot %v does not fund a selected deposit", hash)
		}
		for _, original := range originals {
			if int(original.Index) >= len(fundingTx.TxOut) {
				return nil, false, fmt.Errorf("funding snapshot lacks output %v", original)
			}
		}
		for _, input := range fundingTx.TxIn {
			fundingInputs[input.PreviousOutPoint] = append(
				fundingInputs[input.PreviousOutPoint], hash,
			)
		}
	}

	var before *lndclient.Info
	if len(fundingInputs) > 0 {
		var err error
		before, err = c.client.GetInfo(ctx)
		if err != nil {
			return nil, false, fmt.Errorf("unable to check funding conflict tip: %w", err)
		}
	}

	// We need lnd's wallet transaction view rather than only the funding
	// transaction: a matching previous outpoint tells us the deposit has
	// already been spent by a wallet-known transaction. Use endHeight=-1 so
	// lnd includes unconfirmed transactions and mempool spends.
	txs, err := c.client.ListTransactions(ctx, 0, -1)
	if err != nil {
		return nil, false, err
	}
	stableFundingTip := false
	if len(fundingInputs) > 0 {
		after, err := c.client.GetInfo(ctx)
		if err != nil {
			return nil, false, fmt.Errorf("unable to verify funding conflict tip: %w", err)
		}
		stableFundingTip = before != nil && after != nil &&
			before.SyncedToChain && after.SyncedToChain &&
			before.BlockHeight > 0 && before.BlockHeight == after.BlockHeight &&
			before.BestBlockHash == after.BestBlockHash
	}

	txOuts := make(map[wire.OutPoint]*wire.TxOut, len(outpoints))
	spent := make(map[wire.OutPoint]struct{}, len(outpoints))
	confirmedConflict := false
	for _, tx := range txs {
		for _, prevOutpoint := range tx.PreviousOutpoints {
			outpoint, ok := outpointByString[prevOutpoint.GetOutpoint()]
			if ok {
				spent[outpoint] = struct{}{}
			}
		}

		if tx.Tx == nil {
			continue
		}
		hash := tx.Tx.TxHash()
		for _, input := range tx.Tx.TxIn {
			key := input.PreviousOutPoint.String()
			if outpoint, ok := outpointByString[key]; ok {
				spent[outpoint] = struct{}{}
			}
		}

		// A spend of any original conflicts with every negotiated HTLC,
		// since those spend all originals. Wait for the same depth as the
		// HTLC watcher and reject transactions paying the HTLC script, even
		// when the notifier has not delivered their historical confirmation.
		if len(htlcPkScript) > 0 && tx.Confirmations >= defaultConfTarget {
			paysHtlc := false
			for _, output := range tx.Tx.TxOut {
				if bytes.Equal(output.PkScript, htlcPkScript) {
					paysHtlc = true
					break
				}
			}
			if !paysHtlc {
				for _, input := range tx.Tx.TxIn {
					key := input.PreviousOutPoint.String()
					if _, ok := outpointByString[key]; ok {
						confirmedConflict = true
					}
					for _, fundingHash := range fundingInputs[input.PreviousOutPoint] {
						if stableFundingTip && hash != fundingHash {
							confirmedConflict = true
						}
					}
				}
			}
		}

		for _, outpoint := range outpointsByHash[hash.String()] {
			if int(outpoint.Index) >= len(tx.Tx.TxOut) {
				continue
			}

			txOuts[outpoint] = tx.Tx.TxOut[outpoint.Index]
		}
	}

	for outpoint := range spent {
		delete(txOuts, outpoint)
	}

	return txOuts, confirmedConflict, nil
}
