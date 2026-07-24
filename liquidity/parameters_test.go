package liquidity

import (
	"testing"

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
