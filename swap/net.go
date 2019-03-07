package swap

import (
	"errors"

	"github.com/btcsuite/btcd/chaincfg"
)

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
