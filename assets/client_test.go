package assets

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/lnwire"
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
