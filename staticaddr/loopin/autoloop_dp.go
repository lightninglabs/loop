package loopin

import (
	"errors"
	"math/bits"
	"sort"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/loop/staticaddr/deposit"
)

const (
	// autoloopDPMaxMemoryBytes caps the selector's working set. The
	// selector compresses the sum space when needed so one planning tick
	// cannot consume unbounded memory just because a node has many static
	// deposits.
	autoloopDPMaxMemoryBytes = 128 * 1024 * 1024

	// autoloopDPStateOverheadBytes approximates the per-bucket cost outside
	// of the bitset itself. The exact sum and count slices account for the
	// logical state, and the extra slack keeps the sizing conservative so
	// the selector stays below the intended memory budget in practice.
	autoloopDPStateOverheadBytes = 16
)

var (
	// errAutoloopDPMemoryBudgetTooSmall is returned when even the coarsest
	// two-bucket table would exceed the configured memory budget. This is a
	// structural limitation of the bounded-memory representation, not a
	// liquidity constraint, so callers should not collapse it into
	// ErrNoAutoloopCandidate.
	errAutoloopDPMemoryBudgetTooSmall = errors.New(
		"autoloop dp memory budget too small",
	)
)

// autoloopCandidateDeposit carries the precomputed metadata the DP needs to
// make deterministic comparisons.
type autoloopCandidateDeposit struct {
	// deposit is the original static-address deposit that may be selected.
	deposit *deposit.Deposit

	// residualLife is the remaining lifetime of the deposit in blocks once
	// the current height is taken into account.
	residualLife int64

	// outpoint is cached so deterministic ordering does not keep rebuilding
	// the string form during sort comparisons.
	outpoint string
}

// autoloopDPTable stores one representative subset for each compressed sum
// bucket. Each representative carries its full bitset so later updates can
// compare candidates without relying on mutable predecessor buckets.
//
// This is intentionally heavier than a predecessor-only table. A simpler
// parent-pointer representation would be smaller per bucket, but it becomes
// incorrect once a source bucket is overwritten by a later update because
// already-derived states would silently change their parent chains.
//
// When the selector compresses the sum space, several exact sums can share one
// bucket. The build phase keeps only one representative for that bucket and
// prefers the larger exact sum before the final band scan considers expiry.
// That makes the compressed path approximate: an earlier-expiring smaller-sum
// subset can be hidden by a larger-sum subset in the same bucket. The default
// 128 MiB budget keeps realistic production inputs at step = 1, so this trade-
// off only matters when tests or future callers intentionally lower the memory
// budget.
type autoloopDPTable struct {
	// wordsPerState is the number of 64-bit words needed to represent one
	// deposit-selection bitset.
	wordsPerState int

	// exactSums stores the real satoshi sum of the representative subset in
	// each bucket. The selector never trusts the compressed bucket weight
	// for range checks because the DP is allowed to scale the sum space.
	exactSums []btcutil.Amount

	// counts stores the number of selected deposits for each bucket. A
	// value of -1 marks an unreachable bucket.
	counts []int32

	// selections stores one flattened bitset per bucket. The bitset is the
	// self-contained reconstruction data for the representative subset.
	selections []uint64
}

// selectNoChangeDepositsWithMemoryBudget runs the bounded-memory selector with
// an explicit budget. Tests use this entry point to force the scaling path and
// to exercise hard budget failures deterministically.
func selectNoChangeDepositsWithMemoryBudget(maxAmount, minAmount btcutil.Amount,
	unfilteredDeposits []*deposit.Deposit, csvExpiry, blockHeight uint32,
	excludedOutpoints map[string]struct{},
	maxMemoryBytes int) ([]*deposit.Deposit, error) {

	eligibleDeposits, eligibleTotal := filterAutoloopCandidateDeposits(
		maxAmount, unfilteredDeposits, csvExpiry, blockHeight,
		excludedOutpoints,
	)
	if len(eligibleDeposits) == 0 || eligibleTotal < minAmount {
		return nil, ErrNoAutoloopCandidate
	}

	step, bucketCount, err := autoloopDPSizing(
		maxAmount, len(eligibleDeposits), maxMemoryBytes,
	)
	if err != nil {
		return nil, err
	}

	table := newAutoloopDPTable(bucketCount, len(eligibleDeposits))

	// The empty subset is the base state for all later transitions.
	table.counts[0] = 0

	for depositIndex, candidateDeposit := range eligibleDeposits {
		weight := autoloopDPBucketWeight(
			candidateDeposit.deposit.Value, step,
		)

		// Descending updates preserve the 0-1 constraint: every deposit
		// is either present once in a candidate or not at all.
		start := bucketCount - weight - 1
		for sourceBucket := start; sourceBucket >= 0; sourceBucket-- {
			if !table.isReachable(sourceBucket) {
				continue
			}

			destBucket := sourceBucket + weight
			candidateSum := table.exactSums[sourceBucket] +
				candidateDeposit.deposit.Value

			if candidateSum > maxAmount {
				continue
			}

			beats := table.candidateBeatsState(
				sourceBucket, destBucket, depositIndex,
				candidateSum, eligibleDeposits,
			)
			if !beats {
				continue
			}

			table.copyStateFromSource(
				destBucket, sourceBucket, depositIndex,
				candidateSum,
			)
		}
	}

	bestTotal := btcutil.Amount(-1)
	for bucket := 1; bucket < bucketCount; bucket++ {
		if !table.isReachable(bucket) {
			continue
		}

		exactSum := table.exactSums[bucket]
		if exactSum < minAmount || exactSum > maxAmount {
			continue
		}

		if exactSum > bestTotal {
			bestTotal = exactSum
		}
	}

	if bestTotal < 0 {
		return nil, ErrNoAutoloopCandidate
	}

	// The band lets expiry management influence the final choice, but only
	// after the selector first learns the best liquidity amount that the
	// compressed DP table can achieve. The slack gives back up to 25 percent
	// of the gain above minAmount, not 25 percent of bestTotal itself.
	slack := (bestTotal - minAmount) / 4
	bandFloor := bestTotal - slack

	bestBucket := -1
	for bucket := 1; bucket < bucketCount; bucket++ {
		if !table.isReachable(bucket) {
			continue
		}

		exactSum := table.exactSums[bucket]
		if exactSum < bandFloor || exactSum > bestTotal {
			continue
		}

		if bestBucket == -1 {
			bestBucket = bucket
			continue
		}

		beats := table.stateBeatsStateWithinBand(
			bucket, bestBucket, eligibleDeposits,
		)
		if beats {
			bestBucket = bucket
		}
	}

	if bestBucket == -1 {
		return nil, ErrNoAutoloopCandidate
	}

	selectedIndices := table.selectedIndices(bestBucket)
	selectedDeposits := make([]*deposit.Deposit, 0, len(selectedIndices))
	for _, index := range selectedIndices {
		selectedDeposits = append(
			selectedDeposits, eligibleDeposits[index].deposit,
		)
	}

	return selectedDeposits, nil
}

// filterAutoloopCandidateDeposits removes deposits that can never participate
// in a full-deposit static autoloop suggestion and sorts the remainder in the
// order used by the expiry comparisons.
func filterAutoloopCandidateDeposits(maxAmount btcutil.Amount,
	unfilteredDeposits []*deposit.Deposit, csvExpiry, blockHeight uint32,
	excludedOutpoints map[string]struct{}) (
	[]autoloopCandidateDeposit, btcutil.Amount) {

	eligibleDeposits := make(
		[]autoloopCandidateDeposit, 0, len(unfilteredDeposits),
	)
	var eligibleTotal btcutil.Amount

	for _, candidateDeposit := range unfilteredDeposits {
		outpoint := candidateDeposit.OutPoint.String()
		if _, ok := excludedOutpoints[outpoint]; ok {
			continue
		}

		confirmationHeight := candidateDeposit.GetConfirmationHeight()
		swappable := IsSwappable(
			uint32(confirmationHeight), blockHeight, csvExpiry,
		)
		if !swappable {
			continue
		}

		if candidateDeposit.Value > maxAmount {
			continue
		}

		residualLife := int64(blocksUntilDepositExpiry(
			uint32(confirmationHeight), blockHeight, csvExpiry,
		))

		eligibleDeposits = append(
			eligibleDeposits, autoloopCandidateDeposit{
				deposit:      candidateDeposit,
				residualLife: residualLife,
				outpoint:     outpoint,
			},
		)
		eligibleTotal += candidateDeposit.Value
	}

	// The DP compares reconstructed residual-life sequences directly.
	// Sorting deposits by residual life first keeps those later comparisons
	// exact and deterministic without inventing a scalar "urgency score".
	sort.Slice(eligibleDeposits, func(i, j int) bool {
		left := eligibleDeposits[i]
		right := eligibleDeposits[j]

		switch {
		case left.residualLife != right.residualLife:
			return left.residualLife < right.residualLife

		case left.deposit.Value != right.deposit.Value:
			return left.deposit.Value > right.deposit.Value
		}

		return left.outpoint < right.outpoint
	})

	return eligibleDeposits, eligibleTotal
}

// autoloopDPSizing chooses the smallest bucket step that keeps the compressed
// table within the configured memory budget.
func autoloopDPSizing(maxAmount btcutil.Amount, depositCount,
	maxMemoryBytes int) (btcutil.Amount, int, error) {

	wordsPerState := autoloopDPWordsPerState(depositCount)
	stateBytes := wordsPerState*8 + autoloopDPStateOverheadBytes
	maxBuckets := maxMemoryBytes / stateBytes

	// The selector needs at least bucket zero plus one positive bucket.
	if maxBuckets < 2 {
		return 0, 0, errAutoloopDPMemoryBudgetTooSmall
	}

	step := ceilAmountDiv(maxAmount, btcutil.Amount(maxBuckets-1))
	step = max(step, 1)

	bucketCount := int(ceilAmountDiv(maxAmount, step)) + 1

	return step, bucketCount, nil
}

// autoloopDPWordsPerState returns the number of 64-bit words needed to encode
// one subset bitset for the current deposit count.
func autoloopDPWordsPerState(depositCount int) int {
	return (depositCount + 63) / 64
}

// autoloopDPBucketWeight compresses a deposit value into one DP bucket weight.
//
// The table rounds down, but never below one bucket. Rounding up each deposit
// would accidentally reject some valid exact sums once several per-item
// round-up errors accumulate. The exact-sum guard remains the real safety
// boundary: compressed weights only decide which representative states are kept
// in memory, never whether a candidate is allowed to exceed maxAmount.
func autoloopDPBucketWeight(value, step btcutil.Amount) int {
	weight := int(value / step)
	if weight == 0 {
		return 1
	}

	return weight
}

// ceilAmountDiv performs positive ceiling division for amount sizing.
func ceilAmountDiv(numerator, denominator btcutil.Amount) btcutil.Amount {
	if numerator <= 0 {
		return 0
	}

	return (numerator + denominator - 1) / denominator
}

// newAutoloopDPTable allocates the bounded-memory DP table.
func newAutoloopDPTable(bucketCount, depositCount int) *autoloopDPTable {
	wordsPerState := autoloopDPWordsPerState(depositCount)

	counts := make([]int32, bucketCount)
	for i := range counts {
		counts[i] = -1
	}

	return &autoloopDPTable{
		wordsPerState: wordsPerState,
		exactSums:     make([]btcutil.Amount, bucketCount),
		counts:        counts,
		selections:    make([]uint64, bucketCount*wordsPerState),
	}
}

// isReachable reports whether a bucket currently has a representative subset.
func (t *autoloopDPTable) isReachable(bucket int) bool {
	return t.counts[bucket] >= 0
}

// stateWords returns the flattened bitset slice for one bucket.
func (t *autoloopDPTable) stateWords(bucket int) []uint64 {
	start := bucket * t.wordsPerState
	end := start + t.wordsPerState

	return t.selections[start:end]
}

// copyStateFromSource writes a winning candidate into the destination bucket.
func (t *autoloopDPTable) copyStateFromSource(destBucket, sourceBucket,
	depositIndex int, exactSum btcutil.Amount) {

	destWords := t.stateWords(destBucket)
	sourceWords := t.stateWords(sourceBucket)
	copy(destWords, sourceWords)

	wordIndex := depositIndex / 64
	bitIndex := uint(depositIndex % 64)
	destWords[wordIndex] |= uint64(1) << bitIndex

	t.exactSums[destBucket] = exactSum
	t.counts[destBucket] = t.counts[sourceBucket] + 1
}

// candidateBeatsState reports whether the candidate obtained by extending the
// source bucket with one deposit should replace the destination bucket.
func (t *autoloopDPTable) candidateBeatsState(sourceBucket, destBucket,
	depositIndex int, candidateSum btcutil.Amount,
	deposits []autoloopCandidateDeposit) bool {

	if !t.isReachable(destBucket) {
		return true
	}

	existingSum := t.exactSums[destBucket]
	if candidateSum != existingSum {
		return candidateSum > existingSum
	}

	candidateCount := int(t.counts[sourceBucket]) + 1
	existingCount := int(t.counts[destBucket])
	if candidateCount != existingCount {
		return candidateCount < existingCount
	}

	// Only exact-sum and exact-count ties fall through to expiry
	// comparison. That keeps the expensive residual-life reconstruction
	// off the hot path.
	return t.candidateEarlierThanState(
		sourceBucket, destBucket, depositIndex, deposits,
	)
}

// candidateEarlierThanState compares the candidate residual-life sequence to
// the existing destination sequence.
func (t *autoloopDPTable) candidateEarlierThanState(sourceBucket, destBucket,
	depositIndex int, deposits []autoloopCandidateDeposit) bool {

	candidateResidualLives := t.candidateResidualLives(
		sourceBucket, depositIndex, deposits,
	)
	existingResidualLives := t.stateResidualLives(destBucket, deposits)

	return compareResidualLifeSequences(
		candidateResidualLives, existingResidualLives,
	) < 0
}

// stateBeatsStateWithinBand applies the final band-local ordering:
// 1. earlier-expiring deposits.
// 2. larger exact total.
// 3. fewer deposits.
func (t *autoloopDPTable) stateBeatsStateWithinBand(leftBucket, rightBucket int,
	deposits []autoloopCandidateDeposit) bool {

	leftResidualLives := t.stateResidualLives(leftBucket, deposits)
	rightResidualLives := t.stateResidualLives(rightBucket, deposits)

	cmp := compareResidualLifeSequences(
		leftResidualLives, rightResidualLives,
	)
	switch cmp {
	case -1:
		return true

	case 1:
		return false
	}

	leftSum := t.exactSums[leftBucket]
	rightSum := t.exactSums[rightBucket]
	if leftSum != rightSum {
		return leftSum > rightSum
	}

	return t.counts[leftBucket] < t.counts[rightBucket]
}

// selectedIndices reconstructs the selected deposit indices for a bucket in
// sorted order. The caller must pass a reachable bucket.
func (t *autoloopDPTable) selectedIndices(bucket int) []int {
	count := int(t.counts[bucket])
	if count < 0 {
		panic("selectedIndices called on unreachable bucket")
	}

	selectedIndices := make([]int, 0, count)
	for wordIndex, word := range t.stateWords(bucket) {
		for word != 0 {
			bitIndex := bits.TrailingZeros64(word)
			selectedIndices = append(
				selectedIndices, wordIndex*64+bitIndex,
			)
			word &^= uint64(1) << uint(bitIndex)
		}
	}

	return selectedIndices
}

// candidateResidualLives reconstructs the candidate residual-life sequence.
// The current deposit index is always larger than every index already present
// in the source bucket, so appending preserves the sorted order.
func (t *autoloopDPTable) candidateResidualLives(sourceBucket, depositIndex int,
	deposits []autoloopCandidateDeposit) []int64 {

	residualLives := t.stateResidualLives(sourceBucket, deposits)
	residualLives = append(
		residualLives, deposits[depositIndex].residualLife,
	)

	return residualLives
}

// stateResidualLives reconstructs the sorted residual-life sequence for a
// bucket.
func (t *autoloopDPTable) stateResidualLives(bucket int,
	deposits []autoloopCandidateDeposit) []int64 {

	selectedIndices := t.selectedIndices(bucket)
	residualLives := make([]int64, 0, len(selectedIndices))
	for _, index := range selectedIndices {
		residualLives = append(
			residualLives, deposits[index].residualLife,
		)
	}

	return residualLives
}

// compareResidualLifeSequences compares two sorted residual-life sequences.
//
// A smaller residual-life value means the deposit expires sooner and is thus
// more urgent to consume. The comparison intentionally stops at the shorter
// length: if one sequence is a strict prefix of the other, expiry alone does
// not provide a principled winner and the caller falls back to amount and
// deposit-count tie-breakers instead of inventing an arbitrary preference for
// the longer or shorter set.
func compareResidualLifeSequences(left, right []int64) int {
	limit := min(len(left), len(right))

	for i := range limit {
		switch {
		case left[i] < right[i]:
			return -1

		case left[i] > right[i]:
			return 1
		}
	}

	return 0
}
