package utils

import (
	"bytes"
	"errors"
	"fmt"
	"os"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/zpay32"

	"github.com/btcsuite/btcd/wire"
)

const (
	// FeeRateTotalParts defines the granularity of the fee rate.
	FeeRateTotalParts = 1e6
)

// ShortHash returns a shortened version of the hash suitable for use in
// logging.
func ShortHash(hash *lntypes.Hash) string {
	return hash.String()[:6]
}

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

// GetInvoiceAmt gets the invoice amount. It requires an amount to be specified.
func GetInvoiceAmt(params *chaincfg.Params,
	payReq string) (btcutil.Amount, error) {

	swapPayReq, err := zpay32.Decode(
		payReq, params,
	)
	if err != nil {
		return 0, err
	}

	if swapPayReq.MilliSat == nil {
		return 0, errors.New("no amount in invoice")
	}

	return swapPayReq.MilliSat.ToSatoshis(), nil
}

// FileExists returns true if the file exists, and false otherwise.
func FileExists(path string) bool {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return false
		}
	}

	return true
}

// ChainParamsFromNetwork returns chain parameters based on a network name.
func ChainParamsFromNetwork(network string) (*chaincfg.Params, error) {
	switch network {
	case "mainnet":
		return &chaincfg.MainNetParams, nil
	case "testnet":
		return &chaincfg.TestNet3Params, nil
	case "regtest":
		return &chaincfg.RegressionNetParams, nil
	case "simnet":
		return &chaincfg.SimNetParams, nil
	default:
		return nil, errors.New("unknown network")
	}
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

// CalcFee returns the swap fee for a given swap amount.
func CalcFee(amount, feeBase btcutil.Amount, feeRate int64) btcutil.Amount {
	return feeBase + amount*btcutil.Amount(feeRate)/
		btcutil.Amount(FeeRateTotalParts)
}

// FeeRateAsPercentage converts a feerate to a percentage.
func FeeRateAsPercentage(feeRate int64) float64 {
	return float64(feeRate) / (FeeRateTotalParts / 100)
}
