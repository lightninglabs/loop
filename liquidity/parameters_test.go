package liquidity

import (
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	clientrpc "github.com/lightninglabs/loop/looprpc"
	"github.com/lightninglabs/loop/swap"
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

	p2wpkh, err := btcutil.NewAddressWitnessPubKeyHash(
		make([]byte, 20), params,
	)
	require.NoError(t, err)

	// The compressed secp256k1 generator point serves as a raw public key.
	pubKey, err := hex.DecodeString("0279be667ef9dcbbac55a06295ce870b0" +
		"7029bfcdb2dce28d959f2815b16f81798")
	require.NoError(t, err)

	rawPubKey, err := btcutil.NewAddressPubKey(pubKey, params)
	require.NoError(t, err)

	tests := []struct {
		name string
		addr btcutil.Address
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
