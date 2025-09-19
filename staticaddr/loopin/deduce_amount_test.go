package loopin

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/stretchr/testify/require"
)

// TestDeduceSwapAmount covers all validation branches of DeduceSwapAmount.
func TestDeduceSwapAmount(t *testing.T) {
	dl := lnwallet.DustLimitForSize(input.P2TRSize)

	tests := []struct {
		name      string
		total     btcutil.Amount
		selectAmt btcutil.Amount
		wantAmt   btcutil.Amount
		wantErr   string
	}{
		{
			name:      "negative selected amount",
			total:     dl * 10,
			selectAmt: -1,
			wantErr:   "negative",
		},
		{
			name:      "selected is dust (>0 < dust)",
			total:     dl * 10,
			selectAmt: dl - 1,
			wantErr:   "is dust",
		},
		{
			name:      "total deposit is dust",
			total:     dl - 1,
			selectAmt: 0,
			wantErr:   "total deposit value",
		},
		{
			name:      "selected exceeds total",
			total:     dl * 5,
			selectAmt: dl*5 + 1,
			wantErr:   "exceeds total",
		},
		{
			name:      "leaves dust change",
			total:     dl*5 + (dl - 1),
			selectAmt: dl * 5,
			wantErr:   "leaves dust change",
		},
		{
			name:      "selected zero => swap total",
			total:     dl * 7,
			selectAmt: 0,
			wantAmt:   dl * 7,
		},
		{
			name:      "selected equals total",
			total:     dl * 9,
			selectAmt: dl * 9,
			wantAmt:   dl * 9,
		},
		{
			name:      "selected and remaining both >= dust",
			total:     dl*10 + dl, // 11*dust
			selectAmt: dl * 10,    // remaining = dust
			wantAmt:   dl * 10,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			amt, err := DeduceSwapAmount(tc.total, tc.selectAmt)
			if tc.wantErr != "" {
				require.Error(t, err)
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.wantAmt, amt)
		})
	}
}
