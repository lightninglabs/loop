package utils

import (
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/mempool"
	"github.com/btcsuite/btcd/wire/v2"
)

// DustLimitForPkScript returns the dust limit for a given pkScript. An output
// must be greater or equal to this value.
func DustLimitForPkScript(pkscript []byte) btcutil.Amount {
	return btcutil.Amount(mempool.GetDustThreshold(&wire.TxOut{
		PkScript: pkscript,
	}))
}
