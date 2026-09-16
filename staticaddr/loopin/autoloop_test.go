package loopin

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/staticaddr/script"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

// TestSelectNoChangeDeposits exercises the bounded-memory selector end to end.
// The full decision rule:
//
//  1. build only full-deposit, no-change candidates
//  2. find the best reachable total in the requested range
//  3. allow a band that gives back up to 25 percent of the gain above
//     minAmount
//  4. inside that band, prefer earlier-expiring deposits
//  5. fall back to larger total, then fewer deposits
func TestSelectNoChangeDeposits(t *testing.T) {
	depositSeven := makeAutoloopDeposit(7, 0, 7_000, 200)
	depositFour := makeAutoloopDeposit(4, 0, 4_000, 210)
	depositThreeA := makeAutoloopDeposit(3, 0, 3_000, 220)
	depositThreeB := makeAutoloopDeposit(9, 0, 3_000, 221)
	depositNine := makeAutoloopDeposit(8, 0, 9_000, 205)
	depositFourA := makeAutoloopDeposit(5, 0, 4_000, 215)
	depositFourB := makeAutoloopDeposit(6, 0, 4_000, 216)
	depositOneA := makeAutoloopDeposit(10, 0, 1_000, 230)
	depositOneB := makeAutoloopDeposit(11, 0, 1_000, 231)
	depositOneC := makeAutoloopDeposit(21, 0, 1_000, 232)
	depositFourC := makeAutoloopDeposit(13, 0, 4_000, 200)
	depositFourD := makeAutoloopDeposit(14, 0, 4_000, 201)
	depositFourE := makeAutoloopDeposit(15, 0, 4_000, 220)
	depositFourF := makeAutoloopDeposit(16, 0, 4_000, 221)
	depositFive := makeAutoloopDeposit(17, 0, 5_000, 200)
	depositUnsuitable := makeAutoloopDeposit(18, 0, 6_000, 149)
	depositOversized := makeAutoloopDeposit(19, 0, 9_000, 220)
	depositTwo := makeAutoloopDeposit(20, 0, 2_000, 210)
	depositTen := makeAutoloopDeposit(23, 0, 10_000, 500)

	testCases := []struct {
		name             string
		maxAmount        btcutil.Amount
		minAmount        btcutil.Amount
		deposits         []*deposit.Deposit
		blockHeight      uint32
		excludedOutpoint map[string]struct{}
		expected         []*deposit.Deposit
		expectedErr      error
	}{
		{
			name:      "prefers exact deposit over smaller combo",
			maxAmount: 7_000,
			minAmount: 3_000,
			deposits: []*deposit.Deposit{
				depositSeven, depositFour, depositThreeA,
			},
			blockHeight: 100,
			expected:    []*deposit.Deposit{depositSeven},
		},
		{
			name:      "excluded outpoint falls back to combo",
			maxAmount: 7_000,
			minAmount: 3_000,
			deposits: []*deposit.Deposit{
				depositSeven, depositFour, depositThreeA,
			},
			blockHeight: 100,
			excludedOutpoint: map[string]struct{}{
				depositSeven.OutPoint.String(): {},
			},
			expected: []*deposit.Deposit{
				depositFour, depositThreeA,
			},
		},
		{
			name:      "same total prefers fewer deposits",
			maxAmount: 6_000,
			minAmount: 6_000,
			deposits: []*deposit.Deposit{
				depositFour, depositThreeA, depositThreeB,
				depositOneA, depositOneB,
			},
			blockHeight: 100,
			expected: []*deposit.Deposit{
				depositThreeA, depositThreeB,
			},
		},
		{
			name:      "same total rejects more deposits",
			maxAmount: 2_000,
			minAmount: 2_000,
			deposits: []*deposit.Deposit{
				depositTwo, depositOneA,
				depositOneB, depositOneC,
			},
			blockHeight: 100,
			expected:    []*deposit.Deposit{depositTwo},
		},
		{
			name:      "same total prefers earlier expiries",
			maxAmount: 8_000,
			minAmount: 8_000,
			deposits: []*deposit.Deposit{
				depositFourC, depositFourD,
				depositFourE, depositFourF,
			},
			blockHeight: 100,
			expected: []*deposit.Deposit{
				depositFourC, depositFourD,
			},
		},
		{
			name:      "identical residual lives keep stable pick",
			maxAmount: 8_000,
			minAmount: 8_000,
			deposits: []*deposit.Deposit{
				depositFour, depositThreeA,
				depositFourA, depositThreeB,
			},
			blockHeight: 100,
			expected: []*deposit.Deposit{
				depositFour, depositFourA,
			},
		},
		{
			name:      "filters unswappable and oversized deposits",
			maxAmount: 7_000,
			minAmount: 5_000,
			deposits: []*deposit.Deposit{
				depositFive, depositUnsuitable,
				depositOversized,
			},
			blockHeight: 100,
			expected:    []*deposit.Deposit{depositFive},
		},
		{
			name:      "returns no candidate when all are filtered",
			maxAmount: 7_000,
			minAmount: 5_000,
			deposits: []*deposit.Deposit{
				depositUnsuitable, depositOversized,
			},
			blockHeight: 100,
			expectedErr: ErrNoAutoloopCandidate,
		},
		{
			name:      "returns no candidate below minimum",
			maxAmount: 10_000,
			minAmount: 7_000,
			deposits: []*deposit.Deposit{
				depositFour, depositTwo,
			},
			blockHeight: 100,
			expectedErr: ErrNoAutoloopCandidate,
		},
		{
			name:      "zero minimum finds best positive total",
			maxAmount: 7_000,
			minAmount: 0,
			deposits: []*deposit.Deposit{
				depositFour, depositThreeA,
			},
			blockHeight: 100,
			expected: []*deposit.Deposit{
				depositFour, depositThreeA,
			},
		},
		{
			name:      "deeper search isolates include and exclude",
			maxAmount: 13_000,
			minAmount: 10_000,
			deposits: []*deposit.Deposit{
				depositNine, depositSeven,
				depositFourA, depositFourB,
				depositThreeA,
			},
			blockHeight: 100,
			expected: []*deposit.Deposit{
				depositNine, depositFourA,
			},
		},
		{
			// A slightly smaller total can win when it stays inside
			// the band that gives back only part of the gain above
			// minAmount.
			name: "smaller earlier-expiring combo can win " +
				"inside band",
			maxAmount: 10_000,
			minAmount: 6_000,
			deposits: []*deposit.Deposit{
				depositTen, depositFive, depositFourC,
			},
			blockHeight: 100,
			expected: []*deposit.Deposit{
				depositFive, depositFourC,
			},
		},
		{
			name: "smaller earlier candidate below band does not " +
				"beat best total",
			maxAmount: 9_000,
			minAmount: 8_000,
			deposits: []*deposit.Deposit{
				depositNine, depositSeven,
			},
			blockHeight: 100,
			expected:    []*deposit.Deposit{depositNine},
		},
		{
			name: "returns no candidate when enough value exists " +
				"but no subset fits range",
			maxAmount: 6_000,
			minAmount: 5_000,
			deposits: []*deposit.Deposit{
				depositFourC, depositFourD, depositFourE,
			},
			blockHeight: 100,
			expectedErr: ErrNoAutoloopCandidate,
		},
	}

	selectedOutpoints := func(deposits []*deposit.Deposit) []string {
		result := make([]string, 0, len(deposits))
		for _, selectedDeposit := range deposits {
			result = append(
				result, selectedDeposit.OutPoint.String(),
			)
		}

		return result
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			selectedDeposits, err := selectNoChangeDeposits(
				testCase.maxAmount, testCase.minAmount,
				testCase.deposits,
				testCase.blockHeight, testCase.excludedOutpoint,
			)

			if testCase.expectedErr != nil {
				require.ErrorIs(t, err, testCase.expectedErr)
				require.Nil(t, selectedDeposits)
			} else {
				require.NoError(t, err)
				require.Equal(
					t, selectedOutpoints(testCase.expected),
					selectedOutpoints(selectedDeposits),
				)
			}
		})
	}
}

// TestPrepareAutoloopLoopIn ensures the static manager returns an explicit
// full-deposit request and quotes it with the correct amount and deposit
// count.
func TestPrepareAutoloopLoopIn(t *testing.T) {
	ctx := t.Context()

	selectedDeposit := makeAutoloopDeposit(1, 0, 9_000, 300)

	quoteGetter := &mockQuoteGetter{
		quote: &loop.LoopInQuote{
			SwapFee: 123,
		},
	}

	addressMgr := &mockAddressManager{
		params: &script.Parameters{
			Expiry: 1_000,
		},
	}
	manager, err := NewManager(&Config{
		AddressManager: addressMgr,
		DepositManager: &mockDepositManager{
			activeDeposits: []*deposit.Deposit{selectedDeposit},
		},
		QuoteGetter: quoteGetter,
		NodePubkey:  route.Vertex{2},
	}, 200)
	require.NoError(t, err)

	lastHop := route.Vertex{9}
	request, numDeposits, hasChange, err := manager.PrepareAutoloopLoopIn(
		ctx, lastHop, 5_000, 10_000, "label", "autoloop", nil,
	)
	require.NoError(t, err)

	require.Equal(
		t, []string{selectedDeposit.OutPoint.String()},
		request.DepositOutpoints,
	)
	require.Equal(t, selectedDeposit.Value, request.SelectedAmount)
	require.Equal(t, btcutil.Amount(123), request.MaxSwapFee)
	require.NotNil(t, request.LastHop)
	require.Equal(t, lastHop, *request.LastHop)
	require.Equal(t, "label", request.Label)
	require.Equal(t, "autoloop", request.Initiator)
	require.False(t, request.Fast)
	require.Equal(t, 1, numDeposits)
	require.False(t, hasChange)

	require.Equal(t, selectedDeposit.Value, quoteGetter.amount)
	require.NotNil(t, quoteGetter.lastHop)
	require.Equal(t, lastHop, *quoteGetter.lastHop)
	require.Equal(t, "autoloop", quoteGetter.initiator)
	require.Equal(t, uint32(1), quoteGetter.numDeposits)
	require.False(t, quoteGetter.fast)
	require.Zero(t, addressMgr.getParamsCalls.Load())
}

// TestPrepareAutoloopLoopInExcludedOutpoints verifies that the manager passes
// excluded outpoints through the end-to-end preparation path before quoting
// the candidate.
func TestPrepareAutoloopLoopInExcludedOutpoints(t *testing.T) {
	ctx := t.Context()

	excludedDeposit := makeAutoloopDeposit(1, 0, 9_000, 300)

	selectedDeposit := makeAutoloopDeposit(2, 0, 7_000, 301)

	quoteGetter := &mockQuoteGetter{
		quote: &loop.LoopInQuote{
			SwapFee: 77,
		},
	}

	manager, err := NewManager(&Config{
		AddressManager: &mockAddressManager{
			params: &script.Parameters{
				Expiry: 1_000,
			},
		},
		DepositManager: &mockDepositManager{
			activeDeposits: []*deposit.Deposit{
				excludedDeposit, selectedDeposit,
			},
		},
		QuoteGetter: quoteGetter,
		NodePubkey:  route.Vertex{2},
	}, 200)
	require.NoError(t, err)

	request, numDeposits, hasChange, err := manager.PrepareAutoloopLoopIn(
		ctx, route.Vertex{9}, 5_000, 10_000, "label", "autoloop",
		[]string{excludedDeposit.OutPoint.String()},
	)
	require.NoError(t, err)

	require.Equal(
		t, []string{selectedDeposit.OutPoint.String()},
		request.DepositOutpoints,
	)
	require.Equal(t, selectedDeposit.Value, request.SelectedAmount)
	require.Equal(t, btcutil.Amount(77), request.MaxSwapFee)
	require.Equal(t, 1, numDeposits)
	require.False(t, hasChange)
	require.Equal(t, selectedDeposit.Value, quoteGetter.amount)
}

// makeAutoloopDeposit attaches the address metadata required for selection.
func makeAutoloopDeposit(h byte, index uint32, value btcutil.Amount,
	confirmationHeight int64) *deposit.Deposit {

	d := makeDeposit(h, index, value, confirmationHeight)
	d.AddressParams = &script.Parameters{Expiry: 1_000}
	return d
}

// TestSelectNoChangeDepositsOwnExpiry verifies eligibility and expiry ordering
// use each deposit's owning address rather than a root-address CSV delay.
func TestSelectNoChangeDepositsOwnExpiry(t *testing.T) {
	t.Parallel()

	short := makeAutoloopDeposit(51, 0, 5_000, 200)
	short.AddressParams.Expiry = 1_100
	long := makeAutoloopDeposit(52, 0, 5_000, 100)
	long.AddressParams.Expiry = 1_500
	expiring := makeAutoloopDeposit(53, 0, 6_000, 100)
	expiring.AddressParams.Expiry = 1_000
	missing := makeDeposit(54, 0, 6_000, 200)

	selected, err := selectNoChangeDeposits(
		6_000, 5_000, []*deposit.Deposit{
			long, short, expiring, missing,
		}, 100, nil,
	)
	require.NoError(t, err)
	require.Equal(t, []*deposit.Deposit{short}, selected)
}
