package swap

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
)

// EncodeTx encodes a tx to raw bytes.
func EncodeTx(tx *wire.MsgTx) ([]byte, error) {
	var buffer bytes.Buffer
	err := tx.BtcEncode(&buffer, 0, wire.WitnessEncoding)
	if err != nil {
		return nil, err
	}
	rawTx := buffer.Bytes()

	return rawTx, nil
}

// DecodeTx decodes raw tx bytes.
func DecodeTx(rawTx []byte) (*wire.MsgTx, error) {
	tx := wire.MsgTx{}
	r := bytes.NewReader(rawTx)
	err := tx.BtcDecode(r, 0, wire.WitnessEncoding)
	if err != nil {
		return nil, err
	}

	return &tx, nil
}

// GetScriptOutput locates the given script in the outputs of a transaction and
// returns its outpoint and value.
func GetScriptOutput(htlcTx *wire.MsgTx, scriptHash []byte) (
	*wire.OutPoint, btcutil.Amount, error) {

	for idx, output := range htlcTx.TxOut {
		if bytes.Equal(output.PkScript, scriptHash) {
			return &wire.OutPoint{
				Hash:  htlcTx.TxHash(),
				Index: uint32(idx),
			}, btcutil.Amount(output.Value), nil
		}
	}

	return nil, 0, fmt.Errorf("cannot determine outpoint")
}

// GetTxInputByOutpoint returns a tx input based on a given input outpoint.
func GetTxInputByOutpoint(tx *wire.MsgTx, input *wire.OutPoint) (
	*wire.TxIn, error) {

	for _, in := range tx.TxIn {
		if in.PreviousOutPoint == *input {
			return in, nil
		}
	}

	return nil, errors.New("input not found")
}
