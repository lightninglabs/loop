package utils

import (
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/stretchr/testify/require"
)

// TestIsSupportedDestAddr checks which address types are accepted as swap
// and sweep destinations.
func TestIsSupportedDestAddr(t *testing.T) {
	t.Parallel()

	params := &chaincfg.MainNetParams
	hash20 := make([]byte, 20)
	hash32 := make([]byte, 32)

	// The compressed secp256k1 generator point serves as a raw public key.
	pubKey, err := hex.DecodeString("0279be667ef9dcbbac55a06295ce870b0" +
		"7029bfcdb2dce28d959f2815b16f81798")
	require.NoError(t, err)

	p2pkh, err := btcutil.NewAddressPubKeyHash(hash20, params)
	require.NoError(t, err)

	p2sh, err := btcutil.NewAddressScriptHashFromHash(hash20, params)
	require.NoError(t, err)

	p2wpkh, err := btcutil.NewAddressWitnessPubKeyHash(hash20, params)
	require.NoError(t, err)

	p2wsh, err := btcutil.NewAddressWitnessScriptHash(hash32, params)
	require.NoError(t, err)

	p2tr, err := btcutil.NewAddressTaproot(hash32, params)
	require.NoError(t, err)

	p2pk, err := btcutil.NewAddressPubKey(pubKey, params)
	require.NoError(t, err)

	tests := []struct {
		name      string
		addr      btcutil.Address
		supported bool
	}{
		{name: "p2pkh", addr: p2pkh, supported: true},
		{name: "p2sh", addr: p2sh, supported: true},
		{name: "p2wpkh", addr: p2wpkh, supported: true},
		{name: "p2wsh", addr: p2wsh, supported: true},
		{name: "p2tr", addr: p2tr, supported: true},
		{name: "raw public key", addr: p2pk, supported: false},
		{name: "nil", addr: nil, supported: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(
				t, test.supported, IsSupportedDestAddr(test.addr),
			)
		})
	}
}

// TestCheckAddrNetwork checks that an address is only accepted on its own
// network.
func TestCheckAddrNetwork(t *testing.T) {
	t.Parallel()

	hash20 := make([]byte, 20)

	p2pkh, err := btcutil.NewAddressPubKeyHash(
		hash20, &chaincfg.MainNetParams,
	)
	require.NoError(t, err)

	p2wpkh, err := btcutil.NewAddressWitnessPubKeyHash(
		hash20, &chaincfg.MainNetParams,
	)
	require.NoError(t, err)

	regtestP2WPKH, err := btcutil.NewAddressWitnessPubKeyHash(
		hash20, &chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)

	tests := []struct {
		name   string
		addr   btcutil.Address
		params *chaincfg.Params
		err    error
	}{
		{
			name:   "p2pkh",
			addr:   p2pkh,
			params: &chaincfg.MainNetParams,
		},
		{
			name:   "p2pkh of another network",
			addr:   p2pkh,
			params: &chaincfg.TestNet3Params,
			err:    ErrWrongNetwork,
		},
		{
			name:   "p2wpkh",
			addr:   p2wpkh,
			params: &chaincfg.MainNetParams,
		},
		{
			name:   "p2wpkh of another network",
			addr:   regtestP2WPKH,
			params: &chaincfg.MainNetParams,
			err:    ErrWrongNetwork,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			err := CheckAddrNetwork(test.addr, test.params)
			require.ErrorIs(t, err, test.err)
		})
	}
}
