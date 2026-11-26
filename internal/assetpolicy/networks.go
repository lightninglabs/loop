package policy

import (
	"fmt"

	"github.com/lightninglabs/taproot-assets/address"
)

// RawSimNetHDCoinType is the BIP-0044 coin type used by btcd's native simnet
// parameters. LND deliberately uses the testnet coin type for simnet instead,
// so both forms need explicit, stable definitions.
const RawSimNetHDCoinType uint32 = 115

// CloneAddressParams returns an independent copy of address parameters,
// including the nested Bitcoin network parameters when present.
func CloneAddressParams(params address.ChainParams) *address.ChainParams {
	paramsCopy := params
	if params.Params != nil {
		bitcoinParams := *params.Params
		paramsCopy.Params = &bitcoinParams
	}

	return &paramsCopy
}

// CanonicalAddressParams returns an independent copy of the supported network
// parameters that match the caller's Taproot Asset address tuple of HRP,
// network and HD coin type. Every other field, including the network name that
// selects the proof activation height, comes from the canonical definition.
func CanonicalAddressParams(params *address.ChainParams) (
	*address.ChainParams, error) {

	if params == nil {
		return nil, fmt.Errorf("address parameters are required")
	}
	if params.Params == nil || params.TapHRP == "" {
		return nil, fmt.Errorf("address parameters are incomplete")
	}

	rawSimNet := CloneAddressParams(address.SimNetTap)
	rawSimNet.HDCoinType = RawSimNetHDCoinType
	lndSimNet := CloneAddressParams(address.SimNetTap)
	lndSimNet.HDCoinType = address.TestNet3Tap.HDCoinType

	knownNetworks := []*address.ChainParams{
		&address.MainNetTap,
		&address.TestNet3Tap,
		&address.TestNet4Tap,
		&address.RegressionNetTap,
		&address.SigNetTap,
		rawSimNet,
		lndSimNet,
	}
	for _, known := range knownNetworks {
		if params.TapHRP != known.TapHRP || params.Net != known.Net ||
			params.HDCoinType != known.HDCoinType {

			continue
		}

		return CloneAddressParams(*known), nil
	}

	return nil, fmt.Errorf("unsupported or mismatched address parameters")
}
