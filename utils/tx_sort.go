package utils

import (
	"bytes"

	"github.com/btcsuite/btcd/wire"
)

// Bip69Less is taken from btcd in btcutil/txsort/txsort.go.
func Bip69Less(output1, output2 *wire.TxOut) bool {
	if output1.Value == output2.Value {
		return bytes.Compare(output1.PkScript, output2.PkScript) < 0
	}
	return output1.Value < output2.Value
}
