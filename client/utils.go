package client

import (
	"errors"

	"github.com/btcsuite/btcd/wire"
)

// getTxInputByOutpoint returns a tx input based on a given input outpoint.
func getTxInputByOutpoint(tx *wire.MsgTx, input *wire.OutPoint) (
	*wire.TxIn, error) {

	for _, in := range tx.TxIn {
		if in.PreviousOutPoint == *input {
			return in, nil
		}
	}

	return nil, errors.New("input not found")
}
