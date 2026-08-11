package utils

import (
	"errors"
	"fmt"

	btcaddr "github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
)

// ErrWrongNetwork is returned when an address belongs to another network than
// the active one.
var ErrWrongNetwork = errors.New("address belongs to another network")

// IsSupportedDestAddr returns whether addr has a type that Loop pays swap and
// sweep outputs to: P2PKH, P2SH, P2WPKH, P2WSH or P2TR. It rejects any other
// type that an address decoder may return, such as a raw public key or a
// pay-to-anchor output, which anyone can spend.
func IsSupportedDestAddr(addr btcaddr.Address) bool {
	switch addr.(type) {
	case *btcaddr.AddressPubKeyHash,
		*btcaddr.AddressScriptHash,
		*btcaddr.AddressWitnessPubKeyHash,
		*btcaddr.AddressWitnessScriptHash,
		*btcaddr.AddressTaproot:

		return true

	default:
		return false
	}
}

// CheckAddrNetwork returns an error wrapping ErrWrongNetwork if addr does not
// belong to the network with the given chain parameters. The decoder accepts
// bech32 addresses of all known networks, and their output scripts don't
// depend on the network.
func CheckAddrNetwork(addr btcaddr.Address, params *chaincfg.Params) error {
	if addr.IsForNet(params) {
		return nil
	}

	return fmt.Errorf("%w, the active network is %v", ErrWrongNetwork,
		params.Name)
}
