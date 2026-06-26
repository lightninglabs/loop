package loopin

import (
	"context"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
)

// lndTxOutChecker checks outpoint availability using lnd's wallet transaction
// view. It returns nil for outputs already spent by a wallet-known transaction.
type lndTxOutChecker struct {
	client lndclient.LightningClient
}

// NewLndTxOutChecker creates a TxOutChecker backed by lnd.
func NewLndTxOutChecker(client lndclient.LightningClient) TxOutChecker {
	return &lndTxOutChecker{
		client: client,
	}
}

// GetTxOut returns the tx output if lnd's transaction view still reports the
// outpoint as unspent.
func (c *lndTxOutChecker) GetTxOut(ctx context.Context,
	outpoint wire.OutPoint, includeMempool bool) (*wire.TxOut, error) {

	endHeight := int32(0)
	if includeMempool {
		endHeight = -1
	}

	// We need lnd's wallet transaction view rather than only the funding
	// transaction: a matching previous outpoint tells us the deposit has
	// already been spent by a wallet-known transaction. When mempool spends
	// matter, lnd exposes them through ListTransactions with endHeight=-1.
	txs, err := c.client.ListTransactions(ctx, 0, endHeight)
	if err != nil {
		return nil, err
	}

	outpointStr := outpoint.String()
	for _, tx := range txs {
		for _, prevOutpoint := range tx.PreviousOutpoints {
			if prevOutpoint.GetOutpoint() == outpointStr {
				return nil, nil
			}
		}
	}

	for _, tx := range txs {
		if tx.Tx == nil {
			continue
		}

		txHash := tx.TxHash
		if txHash == "" {
			txHash = tx.Tx.TxHash().String()
		}
		if txHash != outpoint.Hash.String() {
			continue
		}

		if int(outpoint.Index) >= len(tx.Tx.TxOut) {
			return nil, nil
		}

		return tx.Tx.TxOut[outpoint.Index], nil
	}

	return nil, nil
}
