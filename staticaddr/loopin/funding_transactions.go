package loopin

import (
	"bytes"
	"context"
	"fmt"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/staticaddr/deposit"
)

// fundingTransactions snapshots each distinct unconfirmed funding transaction.
// Hashes and selected outputs are checked against the locked deposits before
// trusting the parent inputs as future evidence of a confirmed conflict.
func fundingTransactions(ctx context.Context, client lndclient.LightningClient,
	deposits []*deposit.Deposit) ([]*wire.MsgTx, error) {

	unconfirmed := make(map[chainhash.Hash][]*deposit.Deposit)
	for _, d := range deposits {
		if d.GetConfirmationHeight() <= 0 {
			unconfirmed[d.Hash] = append(unconfirmed[d.Hash], d)
		}
	}
	if len(unconfirmed) == 0 {
		return nil, nil
	}

	txs, err := client.ListTransactions(ctx, 0, -1)
	if err != nil {
		return nil, fmt.Errorf("unable to snapshot deposit funding: %w", err)
	}

	var fundingTxs []*wire.MsgTx
	for _, tx := range txs {
		if tx.Tx == nil {
			continue
		}
		hash := tx.Tx.TxHash()
		selected, ok := unconfirmed[hash]
		if !ok {
			continue
		}
		if err := validateFundingTransaction(tx.Tx); err != nil {
			return nil, err
		}
		for _, d := range selected {
			if int(d.Index) >= len(tx.Tx.TxOut) {
				return nil, fmt.Errorf("funding output %v is missing", d.OutPoint)
			}
			output := tx.Tx.TxOut[d.Index]
			if d.AddressParams == nil || output.Value != int64(d.Value) ||
				!bytes.Equal(output.PkScript, d.AddressParams.PkScript) {

				return nil, fmt.Errorf("funding output %v does not match deposit", d.OutPoint)
			}
		}
		fundingTxs = append(fundingTxs, tx.Tx.Copy())
		delete(unconfirmed, hash)
	}
	if len(unconfirmed) > 0 {
		return nil, fmt.Errorf("missing %d unconfirmed funding transactions",
			len(unconfirmed))
	}

	return fundingTxs, nil
}

// validateFundingTransaction excludes unusable snapshots, especially coinbase
// inputs whose shared null outpoint cannot prove a funding conflict.
func validateFundingTransaction(tx *wire.MsgTx) error {
	if tx == nil || len(tx.TxIn) == 0 || len(tx.TxOut) == 0 {
		return fmt.Errorf("invalid funding transaction snapshot")
	}
	inputs := make(map[wire.OutPoint]struct{}, len(tx.TxIn))
	for _, input := range tx.TxIn {
		if input == nil || (input.PreviousOutPoint.Hash == (chainhash.Hash{}) &&
			input.PreviousOutPoint.Index == ^uint32(0)) {

			return fmt.Errorf("funding snapshot has a null input")
		}
		if _, ok := inputs[input.PreviousOutPoint]; ok {
			return fmt.Errorf("funding snapshot repeats an input")
		}
		inputs[input.PreviousOutPoint] = struct{}{}
	}
	for _, output := range tx.TxOut {
		if output == nil {
			return fmt.Errorf("funding snapshot has a nil output")
		}
	}
	return nil
}
