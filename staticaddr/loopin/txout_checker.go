package loopin

import (
	"context"

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
