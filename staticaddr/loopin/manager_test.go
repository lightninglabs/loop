package loopin

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/stretchr/testify/require"
)

type testCase struct {
	name        string
	deposits    []*deposit.Deposit
	targetValue btcutil.Amount
	csvExpiry   uint32
	blockHeight uint32
	expected    []*deposit.Deposit
	expectedErr string
}

// TestSelectDeposits tests the selectDeposits function, which selects
// deposits that can cover a target value while respecting the dust limit.
func TestSelectDeposits(t *testing.T) {
	d1, d2, d3, d4 := &deposit.Deposit{
		Value:              1_000_000,
		ConfirmationHeight: 1000,
	}, &deposit.Deposit{
		Value:              2_000_000,
		ConfirmationHeight: 2000,
	}, &deposit.Deposit{
		Value:              3_000_000,
		ConfirmationHeight: 3000,
	}, &deposit.Deposit{
		Value:              3_000_000,
		ConfirmationHeight: 3001,
	}
	d1.Hash = chainhash.Hash{1}
	d1.Index = 0
	d2.Hash = chainhash.Hash{2}
	d2.Index = 0
	d3.Hash = chainhash.Hash{3}
	d3.Index = 0
	d4.Hash = chainhash.Hash{4}
	d4.Index = 0

	testCases := []testCase{
		{
			name:        "single deposit exact target",
			deposits:    []*deposit.Deposit{d1},
			targetValue: 1_000_000,
			expected:    []*deposit.Deposit{d1},
			expectedErr: "",
		},
		{
			name:        "prefer larger deposit when both cover",
			deposits:    []*deposit.Deposit{d1, d2},
			targetValue: 1_000_000,
			expected:    []*deposit.Deposit{d2},
			expectedErr: "",
		},
		{
			name:        "prefer largest among three when one is enough",
			deposits:    []*deposit.Deposit{d1, d2, d3},
			targetValue: 1_000_000,
			expected:    []*deposit.Deposit{d3},
			expectedErr: "",
		},
		{
			name:        "single deposit insufficient by 1",
			deposits:    []*deposit.Deposit{d1},
			targetValue: 1_000_001,
			expected:    []*deposit.Deposit{},
			expectedErr: "not enough deposits to cover",
		},
		{
			name:        "target leaves exact dust limit change",
			deposits:    []*deposit.Deposit{d1},
			targetValue: 1_000_000 - dustLimit,
			expected:    []*deposit.Deposit{d1},
			expectedErr: "",
		},
		{
			name:        "target leaves dust change (just over)",
			deposits:    []*deposit.Deposit{d1},
			targetValue: 1_000_000 - dustLimit + 1,
			expected:    []*deposit.Deposit{},
			expectedErr: "not enough deposits to cover",
		},
		{
			name:        "all deposits exactly match target",
			deposits:    []*deposit.Deposit{d1, d2, d3},
			targetValue: d1.Value + d2.Value + d3.Value,
			expected:    []*deposit.Deposit{d1, d2, d3},
			expectedErr: "",
		},
		{
			name:        "sum minus dust limit is allowed (change == dust)",
			deposits:    []*deposit.Deposit{d1, d2, d3},
			targetValue: d1.Value + d2.Value + d3.Value - dustLimit,
			expected:    []*deposit.Deposit{d1, d2, d3},
			expectedErr: "",
		},
		{
			name:        "sum minus dust limit plus 1 is not allowed (dust change)",
			deposits:    []*deposit.Deposit{d1, d2, d3},
			targetValue: d1.Value + d2.Value + d3.Value - dustLimit + 1,
			expected:    []*deposit.Deposit{},
			expectedErr: "not enough deposits to cover",
		},
		{
			name:        "tie by value, prefer earlier expiry",
			deposits:    []*deposit.Deposit{d3, d4},
			targetValue: d4.Value - dustLimit, // d3/d4 have the
			// same value but different expiration.
			expected:    []*deposit.Deposit{d3},
			expectedErr: "",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			selectedDeposits, err := SelectDeposits(
				tc.targetValue, tc.deposits, tc.csvExpiry,
				tc.blockHeight,
			)
			if tc.expectedErr == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.expectedErr)
			}
			require.ElementsMatch(t, tc.expected, selectedDeposits)
		})
	}
}
