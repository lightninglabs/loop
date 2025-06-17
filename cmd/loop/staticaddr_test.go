package main

import (
	"testing"

	"github.com/lightninglabs/loop/looprpc"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/stretchr/testify/require"
)

type testCase struct {
	deposits    []*looprpc.Deposit
	targetValue int64
	expected    []string
	expectedErr string
}

// TestSelectDeposits tests the selectDeposits function, which selects
// deposits that can cover a target value, while respecting the dust limit.
func TestSelectDeposits(t *testing.T) {
	dustLimit := lnwallet.DustLimitForSize(input.P2TRSize)
	d1, d2, d3 := &looprpc.Deposit{
		Value:    1_000_000,
		Outpoint: "1",
	}, &looprpc.Deposit{
		Value:    2_000_000,
		Outpoint: "2",
	}, &looprpc.Deposit{
		Value:    3_000_000,
		Outpoint: "3",
	}

	testCases := []testCase{
		{
			deposits:    []*looprpc.Deposit{d1},
			targetValue: 1_000_000,
			expected:    []string{"1"},
			expectedErr: "",
		},
		{
			deposits:    []*looprpc.Deposit{d1, d2},
			targetValue: 1_000_000,
			expected:    []string{"2"},
			expectedErr: "",
		},
		{
			deposits:    []*looprpc.Deposit{d1, d2, d3},
			targetValue: 1_000_000,
			expected:    []string{"3"},
			expectedErr: "",
		},
		{
			deposits:    []*looprpc.Deposit{d1},
			targetValue: 1_000_001,
			expected:    []string{},
			expectedErr: "not enough deposits to cover",
		},
		{
			deposits:    []*looprpc.Deposit{d1},
			targetValue: int64(1_000_000 - dustLimit),
			expected:    []string{"1"},
			expectedErr: "",
		},
		{
			deposits:    []*looprpc.Deposit{d1},
			targetValue: int64(1_000_000 - dustLimit + 1),
			expected:    []string{},
			expectedErr: "not enough deposits to cover",
		},
		{
			deposits:    []*looprpc.Deposit{d1, d2, d3},
			targetValue: d1.Value + d2.Value + d3.Value,
			expected:    []string{"1", "2", "3"},
			expectedErr: "",
		},
		{
			deposits: []*looprpc.Deposit{d1, d2, d3},
			targetValue: d1.Value + d2.Value + d3.Value -
				int64(dustLimit),
			expected:    []string{"1", "2", "3"},
			expectedErr: "",
		},
		{
			deposits: []*looprpc.Deposit{d1, d2, d3},
			targetValue: d1.Value + d2.Value + d3.Value -
				int64(dustLimit) + 1,
			expected:    []string{},
			expectedErr: "not enough deposits to cover",
		},
	}

	for _, tc := range testCases {
		selectedDeposits, err := selectDeposits(
			tc.deposits, tc.targetValue,
		)
		if tc.expectedErr == "" {
			require.NoError(t, err)
		} else {
			require.ErrorContains(t, err, tc.expectedErr)
		}
		require.ElementsMatch(t, tc.expected, selectedDeposits)
	}
}
