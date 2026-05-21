package main

import (
	"math"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/loop/looprpc"
	"github.com/stretchr/testify/require"
)

// TestResolveMaxSwapFee tests the fee cap resolution logic for static address
// loop-ins covering sat-only, ppm-only, combined caps, and edge cases.
func TestResolveMaxSwapFee(t *testing.T) {
	tests := []struct {
		name      string
		reqAmt    int64
		quotedAmt int64
		quotedFee int64
		satIsSet  bool
		maxSat    uint64
		ppmIsSet  bool
		maxPpm    uint64
		wantFee   btcutil.Amount
		wantErr   string
	}{
		{
			name:      "no override uses quoted fee",
			reqAmt:    500_000,
			quotedFee: 1_000,
			wantFee:   1_000,
		},
		{
			name:      "sat cap above quote forwards cap",
			reqAmt:    500_000,
			quotedFee: 1_000,
			satIsSet:  true,
			maxSat:    2_000,
			wantFee:   2_000,
		},
		{
			name:      "sat cap below quote returns error",
			reqAmt:    500_000,
			quotedFee: 1_000,
			satIsSet:  true,
			maxSat:    500,
			wantErr: "quoted swap fee 1000 sat exceeds maximum " +
				"allowed 500 sat",
		},
		{
			name:      "ppm uses QuotedAmt when req amt is 0",
			quotedAmt: 1_000_000,
			quotedFee: 800,
			ppmIsSet:  true,
			maxPpm:    1_000,
			wantFee:   1_000,
		},
		{
			name:      "both flags set picks tighter sat cap",
			reqAmt:    1_000_000,
			quotedFee: 500,
			satIsSet:  true,
			maxSat:    600,
			ppmIsSet:  true,
			maxPpm:    2_000, // = 2000 sat
			wantFee:   600,
		},
		{
			name:      "both flags set picks tighter ppm cap",
			reqAmt:    1_000_000,
			quotedFee: 500,
			satIsSet:  true,
			maxSat:    5_000,
			ppmIsSet:  true,
			maxPpm:    1_000, // = 1000 sat
			wantFee:   1_000,
		},
		{
			name:      "both flags set quote exceeds tighter cap",
			reqAmt:    1_000_000,
			quotedFee: 700,
			satIsSet:  true,
			maxSat:    5_000,
			ppmIsSet:  true,
			maxPpm:    500, // = 500 sat
			wantErr: "quoted swap fee 700 sat exceeds maximum " +
				"allowed 500 sat",
		},
		{
			name:      "ppm cap rounds to zero returns error",
			reqAmt:    100,
			quotedFee: 1,
			ppmIsSet:  true,
			maxPpm:    1,
			wantErr: "ppm cap rounds to 0 sat for swap amount " +
				"100; use --max_swap_fee_sat instead",
		},
		{
			name:     "explicit zero sat cap rejected",
			reqAmt:   500_000,
			satIsSet: true,
			maxSat:   0,
			wantErr:  "--max_swap_fee_sat must be positive",
		},
		{
			name:     "explicit zero ppm cap rejected",
			reqAmt:   500_000,
			ppmIsSet: true,
			maxPpm:   0,
			wantErr:  "--max_swap_fee_ppm must be positive",
		},
		{
			name:     "sat cap above hard limit rejected",
			reqAmt:   500_000,
			satIsSet: true,
			maxSat:   10_000_001,
			wantErr:  "--max_swap_fee_sat must be <= 10000000",
		},
		{
			name: "max ppm on huge amount defers to tighter " +
				"sat cap",
			reqAmt:    math.MaxInt64,
			quotedFee: 1_000,
			satIsSet:  true,
			maxSat:    2_000,
			ppmIsSet:  true,
			maxPpm:    feePPMBase,
			wantFee:   2_000,
		},
		{
			name:     "ppm above hard limit rejected",
			reqAmt:   math.MaxInt64,
			ppmIsSet: true,
			maxPpm:   feePPMBase + 1,
			wantErr:  "--max_swap_fee_ppm must be <= 1000000",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			quoteReq := &looprpc.QuoteRequest{
				Amt: tc.reqAmt,
			}
			quote := &looprpc.InQuoteResponse{
				SwapFeeSat: tc.quotedFee,
				QuotedAmt:  tc.quotedAmt,
			}

			got, err := resolveMaxSwapFee(
				quoteReq, quote, tc.satIsSet, tc.maxSat,
				tc.ppmIsSet, tc.maxPpm,
			)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
			} else {
				require.NoError(t, err)
				require.Equal(t, tc.wantFee, got)
			}
		})
	}
}

// TestPPMCapForSwapAmount checks that ppm-to-sat conversion rounds down as
// expected and remains correct near the int64 swap-amount limit.
func TestPPMCapForSwapAmount(t *testing.T) {
	tests := []struct {
		name      string
		swapAmt   int64
		maxFeePpm uint64
		want      uint64
	}{
		{
			name:      "one sat at 100 percent",
			swapAmt:   1,
			maxFeePpm: feePPMBase,
			want:      1,
		},
		{
			name:      "rounds down fractional sat",
			swapAmt:   100,
			maxFeePpm: 1,
			want:      0,
		},
		{
			name:      "simple proportional cap",
			swapAmt:   1_000_000,
			maxFeePpm: 1_000,
			want:      1_000,
		},
		{
			name:      "max int64 at 100 percent",
			swapAmt:   math.MaxInt64,
			maxFeePpm: feePPMBase,
			want:      uint64(math.MaxInt64),
		},
		{
			name:      "max int64 at one ppm",
			swapAmt:   math.MaxInt64,
			maxFeePpm: 1,
			want:      uint64(math.MaxInt64) / feePPMBase,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(
				t, tc.want,
				ppmCapForSwapAmount(tc.swapAmt, tc.maxFeePpm),
			)
		})
	}
}
