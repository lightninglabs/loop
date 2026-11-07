package utils

import "github.com/btcsuite/btcd/btcutil"

// IsSupportedDestAddr returns whether addr has a type that Loop pays swap and
// sweep outputs to: P2PKH, P2SH, P2WPKH, P2WSH or P2TR. It rejects any other
// type that an address decoder may return, such as a raw public key or a
// pay-to-anchor output, which anyone can spend.
func IsSupportedDestAddr(addr btcutil.Address) bool {
	switch addr.(type) {
	case *btcutil.AddressPubKeyHash,
		*btcutil.AddressScriptHash,
		*btcutil.AddressWitnessPubKeyHash,
		*btcutil.AddressWitnessScriptHash,
		*btcutil.AddressTaproot:

		return true

	default:
		return false
	}
}
