package tapsdk

import (
	"context"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
)

type scriptWallet struct {
	lndclient.SignerClient

	signature []byte
	failure   error
	calls     int
}

func (w *scriptWallet) SignOutputRawKeyLocator(context.Context, *wire.MsgTx,
	[]*lndclient.SignDescriptor, []*wire.TxOut) ([][]byte, error) {

	w.calls++
	if w.failure != nil {
		err := w.failure
		w.failure = nil
		return nil, err
	}
	return [][]byte{w.signature}, nil
}
