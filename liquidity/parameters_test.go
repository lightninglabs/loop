package liquidity

import (
	"context"
	"encoding/hex"
	"testing"

	btcaddr "github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	clientrpc "github.com/lightninglabs/loop/looprpc"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightninglabs/loop/utils"
	"github.com/stretchr/testify/require"
)

// TestRPCToRuleSwapType verifies RPC swap type conversion.
func TestRPCToRuleSwapType(t *testing.T) {
	tests := []struct {
		name     string
		swapType clientrpc.SwapType
		wantType swap.Type
		wantErr  bool
	}{
		{
			name:     "loop out",
			swapType: clientrpc.SwapType_LOOP_OUT,
			wantType: swap.TypeOut,
		},
		{
			name:     "loop in",
			swapType: clientrpc.SwapType_LOOP_IN,
			wantType: swap.TypeIn,
		},
		{
			name:     "static loop in rejected",
			swapType: clientrpc.SwapType_STATIC_LOOP_IN,
			wantErr:  true,
		},
		{
			name:     "unknown swap type rejected",
			swapType: clientrpc.SwapType(99),
			wantErr:  true,
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			rpcRule := &clientrpc.LiquidityRule{
				Type:              clientrpc.LiquidityRuleType_THRESHOLD,
				IncomingThreshold: 10,
				OutgoingThreshold: 20,
				SwapType:          testCase.swapType,
			}

			got, err := rpcToRule(rpcRule)
			if testCase.wantErr {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			require.Equal(t, testCase.wantType, got.Type)
		})
	}
}

// TestValidateRestrictions tests validating client restrictions against a set
// of server restrictions.
func TestValidateRestrictions(t *testing.T) {
	tests := []struct {
		name   string
		client *Restrictions
		server *Restrictions
		err    error
	}{
		{
			name: "client invalid",
			client: &Restrictions{
				Minimum: 100,
				Maximum: 1,
			},
			server: testRestrictions,
			err:    ErrMinimumExceedsMaximumAmt,
		},
		{
			name: "maximum exceeds server",
			client: &Restrictions{
				Maximum: 2000,
			},
			server: &Restrictions{
				Minimum: 1000,
				Maximum: 1500,
			},
			err: ErrMaxExceedsServer,
		},
		{
			name: "minimum less than server",
			client: &Restrictions{
				Minimum: 500,
			},
			server: &Restrictions{
				Minimum: 1000,
				Maximum: 1500,
			},
			err: ErrMinLessThanServer,
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			err := validateRestrictions(
				testCase.server, testCase.client,
			)
			require.Equal(t, testCase.err, err)
		})
	}
}

// TestParametersRejectUnsupportedDestAddr verifies that autoloop cannot sweep
// to a destination type that loop out rejects.
func TestParametersRejectUnsupportedDestAddr(t *testing.T) {
	params := &chaincfg.MainNetParams

	p2wpkh, err := btcaddr.NewAddressWitnessPubKeyHash(
		make([]byte, 20), params,
	)
	require.NoError(t, err)

	// The compressed secp256k1 generator point serves as a raw public key.
	pubKey, err := hex.DecodeString("0279be667ef9dcbbac55a06295ce870b0" +
		"7029bfcdb2dce28d959f2815b16f81798")
	require.NoError(t, err)

	rawPubKey, err := btcaddr.NewAddressPubKey(pubKey, params)
	require.NoError(t, err)

	p2a, err := btcaddr.NewAddressPayToAnchor(params)
	require.NoError(t, err)

	tests := []struct {
		name string
		addr btcaddr.Address
		err  error
	}{
		{
			name: "p2wpkh",
			addr: p2wpkh,
		},
		{
			name: "raw public key",
			addr: rawPubKey,
			err:  ErrUnsupportedDestAddr,
		},
		{
			name: "pay to anchor",
			addr: p2a,
			err:  ErrUnsupportedDestAddr,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg, _ := newTestConfig()
			manager := NewManager(cfg)

			newParams := defaultParameters
			newParams.DestAddr = test.addr

			err := manager.setParameters(t.Context(), newParams)
			require.ErrorIs(t, err, test.err)
		})
	}
}

// TestSetParametersDestAddr verifies that the autoloop destination address is
// decoded and checked for the active network. Base58 addresses and raw public
// keys used to crash loopd, because the decoder dereferences the chain
// parameters for them.
func TestSetParametersDestAddr(t *testing.T) {
	_, lnd := newTestConfig()
	params := lnd.ChainParams

	p2pkh, err := btcaddr.NewAddressPubKeyHash(make([]byte, 20), params)
	require.NoError(t, err)

	p2sh, err := btcaddr.NewAddressScriptHashFromHash(
		make([]byte, 20), params,
	)
	require.NoError(t, err)

	p2wpkh, err := btcaddr.NewAddressWitnessPubKeyHash(
		make([]byte, 20), params,
	)
	require.NoError(t, err)

	mainnetP2PKH, err := btcaddr.NewAddressPubKeyHash(
		make([]byte, 20), &chaincfg.MainNetParams,
	)
	require.NoError(t, err)

	mainnetP2WPKH, err := btcaddr.NewAddressWitnessPubKeyHash(
		make([]byte, 20), &chaincfg.MainNetParams,
	)
	require.NoError(t, err)

	tests := []struct {
		name     string
		addr     string
		destAddr btcaddr.Address
		err      error
	}{
		{
			name:     "p2pkh",
			addr:     p2pkh.EncodeAddress(),
			destAddr: p2pkh,
		},
		{
			name:     "p2sh",
			addr:     p2sh.EncodeAddress(),
			destAddr: p2sh,
		},
		{
			// The compressed secp256k1 generator point.
			name: "raw public key",
			addr: "0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce" +
				"28d959f2815b16f81798",
			err: ErrUnsupportedDestAddr,
		},
		{
			name:     "p2wpkh",
			addr:     p2wpkh.EncodeAddress(),
			destAddr: p2wpkh,
		},
		{
			name: "p2pkh of another network",
			addr: mainnetP2PKH.EncodeAddress(),
			err:  btcaddr.ErrUnknownAddressType,
		},
		{
			name: "p2wpkh of another network",
			addr: mainnetP2WPKH.EncodeAddress(),
			err:  utils.ErrWrongNetwork,
		},
		{
			name: "default",
			addr: "default",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg, _ := newTestConfig()
			cfg.PutLiquidityParams = func(context.Context,
				[]byte) error {

				return nil
			}
			manager := NewManager(cfg)

			req, err := ParametersToRpc(defaultParameters)
			require.NoError(t, err)
			req.AutoloopDestAddress = test.addr

			err = manager.SetParameters(t.Context(), req)
			require.ErrorIs(t, err, test.err)
			if test.err != nil {
				return
			}

			require.Equal(
				t, test.destAddr, manager.GetParameters().DestAddr,
			)
		})
	}
}
