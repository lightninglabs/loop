package reservation

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEstimatePrepayRoutingFee(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		fee, prepay, main, want uint64
		wantErr                 bool
	}{
		{
			name:   "proportional",
			fee:    10_000,
			prepay: 11_000,
			main:   10_000_000,
			want:   11,
		},
		{
			name:   "round up",
			fee:    10,
			prepay: 11_000,
			main:   10_000_000,
			want:   1,
		},
		{
			name:   "free route",
			prepay: 11_000,
			main:   10_000_000,
		},
		{
			name:   "larger prepay",
			fee:    10,
			prepay: 200,
			main:   100,
			want:   20,
		},
		{
			name:   "wide product",
			fee:    math.MaxInt64,
			prepay: math.MaxInt64,
			main:   math.MaxInt64,
			want:   math.MaxInt64,
		},
		{
			name:    "result out of range",
			fee:     math.MaxInt64,
			prepay:  2,
			main:    1,
			wantErr: true,
		},
		{
			name:    "missing main",
			fee:     1,
			prepay:  1,
			wantErr: true,
		},
		{
			name:    "missing prepay",
			fee:     1,
			main:    1,
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := estimatePrepayRoutingFee(tc.fee, tc.prepay, tc.main)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}
