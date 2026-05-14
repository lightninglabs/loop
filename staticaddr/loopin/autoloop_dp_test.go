package loopin

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/stretchr/testify/require"
)

// TestSelectNoChangeDepositsWithMemoryBudget covers the dp-specific behavior
// that the default helper does not expose directly: forced scaling and hard
// budget failures.
func TestSelectNoChangeDepositsWithMemoryBudget(t *testing.T) {
	t.Parallel()

	depositSeven := makeDeposit(31, 0, 7_000, 240)
	depositFour := makeDeposit(32, 0, 4_000, 241)
	depositThree := makeDeposit(33, 0, 3_000, 242)

	testCases := []struct {
		name          string
		maxAmount     btcutil.Amount
		minAmount     btcutil.Amount
		deposits      []*deposit.Deposit
		maxMemory     int
		expected      []*deposit.Deposit
		expectedError error
	}{
		{
			name: "a tight but sufficient budget forces scaled " +
				"buckets and still finds the only valid subset",
			maxAmount: 10_000,
			minAmount: 9_000,
			deposits: []*deposit.Deposit{
				depositSeven, depositFour, depositThree,
			},
			maxMemory: 240,
			expected: []*deposit.Deposit{
				depositSeven, depositThree,
			},
		},
		{
			name: "a budget smaller than two state buckets fails " +
				"explicitly instead of pretending no " +
				"candidate exists",
			maxAmount: 10_000,
			minAmount: 9_000,
			deposits: []*deposit.Deposit{
				depositSeven, depositThree,
			},
			maxMemory:     23,
			expectedError: errAutoloopDPMemoryBudgetTooSmall,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			deposits, err := selectNoChangeDepositsWithMemoryBudget(
				testCase.maxAmount, testCase.minAmount,
				testCase.deposits, 1_000, 100, nil,
				testCase.maxMemory,
			)

			if testCase.expectedError != nil {
				require.ErrorIs(t, err, testCase.expectedError)
				require.Nil(t, deposits)

				return
			}

			require.NoError(t, err)
			require.Equal(
				t, depositOutpoints(testCase.expected),
				depositOutpoints(deposits),
			)
		})
	}
}

// TestAutoloopDPSizing verifies the bucket sizing math. These cases are easier
// to understand directly than by inferring the step from a larger selector
// behavior test.
func TestAutoloopDPSizing(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name            string
		maxAmount       btcutil.Amount
		depositCount    int
		maxMemoryBytes  int
		expectedStep    btcutil.Amount
		expectedBuckets int
		expectedWords   int
		expectedError   error
	}{
		{
			name: "when the table fits exactly the step remains " +
				"one",
			maxAmount:       100,
			depositCount:    1,
			maxMemoryBytes:  24 * 200,
			expectedStep:    1,
			expectedBuckets: 101,
			expectedWords:   1,
		},
		{
			name: "when memory is tight the step increases just " +
				"enough to stay inside budget",
			maxAmount:       100,
			depositCount:    64,
			maxMemoryBytes:  24 * 11,
			expectedStep:    10,
			expectedBuckets: 11,
			expectedWords:   1,
		},
		{
			name: "when the budget cannot hold bucket zero and " +
				"one positive bucket, sizing fails",
			maxAmount:      100,
			depositCount:   64,
			maxMemoryBytes: 23,
			expectedError:  errAutoloopDPMemoryBudgetTooSmall,
			expectedWords:  1,
		},
		{
			name: "a non-positive max amount still rounds the " +
				"step up to one after ceiling division " +
				"returns zero",
			maxAmount:       0,
			depositCount:    64,
			maxMemoryBytes:  24 * 10,
			expectedStep:    1,
			expectedBuckets: 1,
			expectedWords:   1,
		},
		{
			name: "when the deposit count exceeds one word the " +
				"state size rounds up to the next word",
			maxAmount:       100,
			depositCount:    65,
			maxMemoryBytes:  32 * 50,
			expectedStep:    3,
			expectedBuckets: 35,
			expectedWords:   2,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(
				t, testCase.expectedWords,
				autoloopDPWordsPerState(testCase.depositCount),
			)

			step, bucketCount, err := autoloopDPSizing(
				testCase.maxAmount, testCase.depositCount,
				testCase.maxMemoryBytes,
			)

			if testCase.expectedError != nil {
				require.ErrorIs(t, err, testCase.expectedError)
				require.Zero(t, step)
				require.Zero(t, bucketCount)

				return
			}

			require.NoError(t, err)
			require.Equal(t, testCase.expectedStep, step)
			require.Equal(t, testCase.expectedBuckets, bucketCount)
		})
	}
}

// TestAutoloopDPBucketWeight verifies the compressed bucket mapping directly.
// The selector relies on this helper to avoid the round-up bug where several
// individually rounded deposits can make a valid exact sum unreachable.
func TestAutoloopDPBucketWeight(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		value    btcutil.Amount
		step     btcutil.Amount
		expected int
	}{
		{
			name: "values larger than the step are truncated " +
				"into the matching floor bucket",
			value:    10,
			step:     3,
			expected: 3,
		},
		{
			name: "values smaller than the step still consume " +
				"one bucket so they remain selectable",
			value:    2,
			step:     5,
			expected: 1,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(
				t, testCase.expected,
				autoloopDPBucketWeight(
					testCase.value, testCase.step,
				),
			)
		})
	}
}

// TestCompareResidualLifeSequences isolates the expiry-order helper. This is
// the selector's "what expires sooner?" rule, so the cases state explicitly
// why the helper should consider one sequence earlier, later, or tied.
func TestCompareResidualLifeSequences(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		left     []int64
		right    []int64
		expected int
	}{
		{
			name: "the left sequence is earlier when the first " +
				"differing deposit expires sooner",
			left:     []int64{100, 150},
			right:    []int64{100, 200},
			expected: -1,
		},
		{
			name: "the right sequence is earlier when its first " +
				"differing deposit expires sooner",
			left:     []int64{150, 250},
			right:    []int64{150, 200},
			expected: 1,
		},
		{
			name: "a strict prefix is treated as an expiry tie " +
				"so later amount and count rules can decide",
			left:     []int64{100},
			right:    []int64{100, 200},
			expected: 0,
		},
		{
			name: "the same strict-prefix rule applies " +
				"regardless of which side is longer",
			left:     []int64{100, 200},
			right:    []int64{100},
			expected: 0,
		},
		{
			name: "identical residual-life sequences compare as " +
				"equal",
			left:     []int64{100, 200},
			right:    []int64{100, 200},
			expected: 0,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			result := compareResidualLifeSequences(
				testCase.left, testCase.right,
			)
			require.Equal(t, testCase.expected, result)
		})
	}
}

// TestFilterAutoloopCandidateDeposits covers the low-level filter and sort
// helper so selector tests do not have to infer ordering rules indirectly.
func TestFilterAutoloopCandidateDeposits(t *testing.T) {
	t.Parallel()

	earlierSameValue := makeDeposit(41, 0, 5_000, 200)
	laterSameValueA := makeDeposit(42, 0, 5_000, 210)
	laterSameValueB := makeDeposit(43, 0, 5_000, 210)
	oversized := makeDeposit(44, 0, 9_000, 220)
	unswappable := makeDeposit(45, 0, 4_000, 149)

	selectedDeposits, total := filterAutoloopCandidateDeposits(
		7_000,
		[]*deposit.Deposit{
			laterSameValueB, oversized, earlierSameValue,
			unswappable, laterSameValueA,
		},
		1_000, 100, nil,
	)

	require.Equal(t, btcutil.Amount(15_000), total)
	require.Equal(
		t,
		[]string{
			earlierSameValue.OutPoint.String(),
			laterSameValueA.OutPoint.String(),
			laterSameValueB.OutPoint.String(),
		},
		candidateOutpoints(selectedDeposits),
	)
}

// TestAutoloopDPComparators isolates helper branches that are awkward to hit
// predictably through the full selector alone.
func TestAutoloopDPComparators(t *testing.T) {
	t.Parallel()

	makeCandidateDeposits := func(
		deposits ...*deposit.Deposit) []autoloopCandidateDeposit {

		candidates := make(
			[]autoloopCandidateDeposit, 0, len(deposits),
		)
		for _, deposit := range deposits {
			candidate := autoloopCandidateDeposit{
				deposit:      deposit,
				residualLife: deposit.ConfirmationHeight,
				outpoint:     deposit.OutPoint.String(),
			}
			candidates = append(candidates, candidate)
		}

		return candidates
	}

	t.Run("candidateBeatsState prefers a larger exact sum in the same "+
		"bucket",
		func(t *testing.T) {

			t.Parallel()

			small := makeDeposit(51, 0, 4_000, 300)
			medium := makeDeposit(52, 0, 5_000, 301)
			later := makeDeposit(53, 0, 1_000, 302)

			candidates := makeCandidateDeposits(
				small, medium, later,
			)
			table := newAutoloopDPTable(3, len(candidates))
			table.counts[0] = 0
			table.copyStateFromSource(1, 0, 0, small.Value)
			table.copyStateFromSource(2, 0, 1, medium.Value)

			require.True(t, table.candidateBeatsState(
				2, 1, 2, medium.Value+later.Value, candidates,
			))
		},
	)

	t.Run("stateBeatsStateWithinBand falls back to sum when expiry ties",
		func(t *testing.T) {
			t.Parallel()

			six := makeDeposit(54, 0, 6_000, 300)
			five := makeDeposit(55, 0, 5_000, 300)

			candidates := makeCandidateDeposits(six, five)
			table := newAutoloopDPTable(3, len(candidates))
			table.counts[0] = 0
			table.copyStateFromSource(1, 0, 0, six.Value)
			table.copyStateFromSource(2, 0, 1, five.Value)

			require.True(t, table.stateBeatsStateWithinBand(
				1, 2, candidates,
			))
		},
	)

	t.Run("stateBeatsStateWithinBand falls back to fewer deposits "+
		"when expiry and sum tie",
		func(t *testing.T) {
			t.Parallel()

			six := makeDeposit(56, 0, 6_000, 300)
			five := makeDeposit(57, 0, 5_000, 300)
			one := makeDeposit(58, 0, 1_000, 300)

			candidates := makeCandidateDeposits(six, five, one)
			table := newAutoloopDPTable(4, len(candidates))
			table.counts[0] = 0
			table.copyStateFromSource(1, 0, 0, six.Value)
			table.copyStateFromSource(2, 0, 1, five.Value)
			table.copyStateFromSource(3, 2, 2, five.Value+one.Value)

			require.True(t, table.stateBeatsStateWithinBand(
				1, 3, candidates,
			))
		},
	)
}

// depositOutpoints turns a deposit set into a stable, readable assertion
// surface for the selector tests.
func depositOutpoints(deposits []*deposit.Deposit) []string {
	outpoints := make([]string, 0, len(deposits))
	for _, selectedDeposit := range deposits {
		outpoints = append(outpoints, selectedDeposit.OutPoint.String())
	}

	return outpoints
}

// candidateOutpoints exposes the filtered candidate order in a readable form.
func candidateOutpoints(
	deposits []autoloopCandidateDeposit) []string {

	outpoints := make([]string, 0, len(deposits))
	for _, candidateDeposit := range deposits {
		outpoints = append(outpoints, candidateDeposit.outpoint)
	}

	return outpoints
}
