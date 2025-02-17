package assets

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/taproot-assets/taprpc/rfqrpc"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

func TestGetPaymentMaxAmount(t *testing.T) {
	tests := []struct {
		satAmount          btcutil.Amount
		feeLimitMultiplier float64
		expectedAmount     lnwire.MilliSatoshi
		expectError        bool
	}{
		{
			satAmount:          btcutil.Amount(250000),
			feeLimitMultiplier: 1.2,
			expectedAmount:     lnwire.MilliSatoshi(300000000),
			expectError:        false,
		},
		{
			satAmount:          btcutil.Amount(100000),
			feeLimitMultiplier: 1.5,
			expectedAmount:     lnwire.MilliSatoshi(150000000),
			expectError:        false,
		},
		{
			satAmount:          btcutil.Amount(50000),
			feeLimitMultiplier: 2.0,
			expectedAmount:     lnwire.MilliSatoshi(100000000),
			expectError:        false,
		},
		{
			satAmount:          btcutil.Amount(0),
			feeLimitMultiplier: 1.2,
			expectedAmount:     lnwire.MilliSatoshi(0),
			expectError:        true,
		},
		{
			satAmount:          btcutil.Amount(250000),
			feeLimitMultiplier: 0.8,
			expectedAmount:     lnwire.MilliSatoshi(0),
			expectError:        true,
		},
	}

	for _, test := range tests {
		result, err := getPaymentMaxAmount(
			test.satAmount, test.feeLimitMultiplier,
		)
		if test.expectError {
			if err == nil {
				t.Fatalf("expected error but got none")
			}
		} else {
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result != test.expectedAmount {
				t.Fatalf("expected %v, got %v",
					test.expectedAmount, result)
			}
		}
	}
}

func TestGetSatsFromAssetAmt(t *testing.T) {
	tests := []struct {
		assetAmt    uint64
		assetRate   *rfqrpc.FixedPoint
		expected    btcutil.Amount
		expectError bool
	}{
		{
			assetAmt:    1000,
			assetRate:   &rfqrpc.FixedPoint{Coefficient: "100000", Scale: 0},
			expected:    btcutil.Amount(1000000),
			expectError: false,
		},
		{
			assetAmt:    500000,
			assetRate:   &rfqrpc.FixedPoint{Coefficient: "200000000", Scale: 0},
			expected:    btcutil.Amount(250000),
			expectError: false,
		},
		{
			assetAmt:    0,
			assetRate:   &rfqrpc.FixedPoint{Coefficient: "100000000", Scale: 0},
			expected:    btcutil.Amount(0),
			expectError: false,
		},
	}

	for _, test := range tests {
		result, err := getSatsFromAssetAmt(test.assetAmt, test.assetRate)
		if test.expectError {
			require.NotNil(t, err)
		} else {
			require.Nil(t, err)
			require.Equal(t, test.expected, result)
		}
	}
}
