package loopin

import (
	"bytes"
	"context"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
)

// lndTxOutChecker checks outpoints against lnd's wallet transaction view. It
// reports outputs that a wallet transaction creates and none spends, and
// whether a wallet transaction that spends one of them pays to a given
// pkScript.
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

	outpointByString := make(map[string]wire.OutPoint, len(outpoints))
	outpointsByHash := make(map[string][]wire.OutPoint, len(outpoints))
	for _, outpoint := range outpoints {
		outpointByString[outpoint.String()] = outpoint
		outpointsByHash[outpoint.Hash.String()] = append(
			outpointsByHash[outpoint.Hash.String()], outpoint,
		)
	}

	// We need lnd's wallet transaction view rather than only the funding
	// transaction: a matching previous outpoint tells us the deposit has
	// already been spent by a wallet-known transaction. Use endHeight=-1 so
	// lnd includes unconfirmed transactions and mempool spends.
	txs, err := c.client.ListTransactions(ctx, 0, -1)
	if err != nil {
		return nil, err
	}

	txOuts := make(map[wire.OutPoint]*wire.TxOut, len(outpoints))
	spent := make(map[wire.OutPoint]struct{}, len(outpoints))
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

		txHash := tx.TxHash
		if txHash == "" {
			txHash = tx.Tx.TxHash().String()
		}

		for _, outpoint := range outpointsByHash[txHash] {
			if int(outpoint.Index) >= len(tx.Tx.TxOut) {
				continue
			}

			txOuts[outpoint] = tx.Tx.TxOut[outpoint.Index]
		}
	}

	for outpoint := range spent {
		delete(txOuts, outpoint)
	}

	return txOuts, nil
}

// SpentByTxPayingTo reports whether lnd's wallet lists a transaction that
// spends one of the outpoints and has an output with the given pkScript. It
// only lists unconfirmed transactions and transactions confirmed at or above
// startHeight, so the lookup doesn't decode the whole wallet history.
func (c *lndTxOutChecker) SpentByTxPayingTo(ctx context.Context,
	outpoints []wire.OutPoint, pkScript []byte,
	startHeight int32) (bool, error) {

	wanted := make(map[wire.OutPoint]struct{}, len(outpoints))
	for _, outpoint := range outpoints {
		wanted[outpoint] = struct{}{}
	}

	txs, err := c.client.ListTransactions(ctx, startHeight, -1)
	if err != nil {
		return false, err
	}

	for _, tx := range txs {
		if tx.Tx == nil || !paysTo(tx.Tx, pkScript) {
			continue
		}

		for _, txIn := range tx.Tx.TxIn {
			if _, ok := wanted[txIn.PreviousOutPoint]; ok {
				return true, nil
			}
		}
	}

	return false, nil
}

// paysTo reports whether the transaction has an output with the pkScript.
func paysTo(tx *wire.MsgTx, pkScript []byte) bool {
	for _, txOut := range tx.TxOut {
		if bytes.Equal(txOut.PkScript, pkScript) {
			return true
		}
	}

	return false
}
