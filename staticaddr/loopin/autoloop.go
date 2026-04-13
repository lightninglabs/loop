package loopin

import (
	"context"
	"errors"
	"slices"
	"sort"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightningnetwork/lnd/routing/route"
)

var (
	// ErrNoAutoloopCandidate is returned when the static-address side
	// cannot build a full-deposit, no-change loop-in candidate that fits
	// the planner's requested amount bounds.
	ErrNoAutoloopCandidate = errors.New("no autoloop candidate")
)

// PrepareAutoloopLoopIn builds a static-address loop-in request for autoloop
// without dispatching it. The returned request always uses full deposits,
// explicit outpoints, and an explicit selected amount, so the caller can
// account for the suggestion without depending on static-address internals.
func (m *Manager) PrepareAutoloopLoopIn(ctx context.Context,
	lastHop route.Vertex, minAmount, maxAmount btcutil.Amount, label,
	initiator string, excludedOutpoints []string) (
	*loop.StaticAddressLoopInRequest, int, bool, error) {

	if minAmount <= 0 || maxAmount < minAmount {
		return nil, 0, false, ErrNoAutoloopCandidate
	}

	allDeposits, err := m.cfg.DepositManager.GetActiveDepositsInState(
		deposit.Deposited,
	)
	if err != nil {
		return nil, 0, false, err
	}

	params, err := m.cfg.AddressManager.GetStaticAddressParameters(ctx)
	if err != nil {
		return nil, 0, false, err
	}

	excluded := make(map[string]struct{}, len(excludedOutpoints))
	for _, outpoint := range excludedOutpoints {
		excluded[outpoint] = struct{}{}
	}

	selectedDeposits, err := selectNoChangeDeposits(
		maxAmount, minAmount, allDeposits, params.Expiry,
		m.currentHeight.Load(), excluded,
	)
	if err != nil {
		return nil, 0, false, err
	}

	selectedAmount := sumOfDeposits(selectedDeposits)
	quote, err := m.cfg.QuoteGetter.GetLoopInQuote(
		ctx, selectedAmount, m.cfg.NodePubkey, &lastHop, nil,
		initiator, uint32(len(selectedDeposits)), false,
	)
	if err != nil {
		return nil, 0, false, err
	}

	outpoints := make([]string, 0, len(selectedDeposits))
	for _, selectedDeposit := range selectedDeposits {
		outpoints = append(outpoints, selectedDeposit.OutPoint.String())
	}

	request := &loop.StaticAddressLoopInRequest{
		DepositOutpoints: outpoints,
		SelectedAmount:   selectedAmount,
		MaxSwapFee:       quote.SwapFee,
		LastHop:          &lastHop,
		Label:            label,
		Initiator:        initiator,
		Fast:             false,
	}

	return request, len(selectedDeposits), false, nil
}

// selectNoChangeDeposits chooses the highest-value swappable deposit set whose
// full value stays within the requested range. The selector never creates
// change, so the returned set's total is the actual swap amount.
func selectNoChangeDeposits(maxAmount, minAmount btcutil.Amount,
	unfilteredDeposits []*deposit.Deposit, csvExpiry, blockHeight uint32,
	excludedOutpoints map[string]struct{}) ([]*deposit.Deposit, error) {

	// Filter out deposits that cannot safely participate in a loop-in or
	// were already allocated to a larger suggestion earlier in the same
	// planning pass.
	deposits := make([]*deposit.Deposit, 0, len(unfilteredDeposits))
	for _, deposit := range unfilteredDeposits {
		if _, ok := excludedOutpoints[deposit.OutPoint.String()]; ok {
			continue
		}

		swappable := IsSwappable(
			uint32(deposit.ConfirmationHeight), blockHeight,
			csvExpiry,
		)
		if !swappable {
			continue
		}

		if deposit.Value > maxAmount {
			continue
		}

		deposits = append(deposits, deposit)
	}

	if len(deposits) == 0 {
		return nil, ErrNoAutoloopCandidate
	}

	// Sort by value so the search finds large feasible totals early. The
	// expiry tie-break keeps equal-value deposits deterministic and helps
	// the later candidate comparison prefer sooner-expiring funds.
	sort.SliceStable(deposits, func(i, j int) bool {
		if deposits[i].Value == deposits[j].Value {
			return deposits[i].ConfirmationHeight <
				deposits[j].ConfirmationHeight
		}

		return deposits[i].Value > deposits[j].Value
	})

	// Precompute a suffix sum so branches that cannot possibly beat the
	// current best total can be pruned before exploring the expensive part
	// of the search tree.
	suffixSums := make([]btcutil.Amount, len(deposits)+1)
	for i := len(deposits) - 1; i >= 0; i-- {
		suffixSums[i] = suffixSums[i+1] + deposits[i].Value
	}

	var (
		bestSelection []int
		bestTotal     btcutil.Amount
	)

	// betterSelection applies the full-deposit ordering:
	// 1. highest total not exceeding the target
	// 2. fewer deposits
	// 3. earlier-expiring deposits
	betterSelection := func(candidate []int, total btcutil.Amount) bool {
		switch {
		case total > bestTotal:
			return true

		case total < bestTotal:
			return false

		case bestSelection == nil:
			return true

		case len(candidate) < len(bestSelection):
			return true

		case len(candidate) > len(bestSelection):
			return false
		}

		// Use signed arithmetic here so an expired deposit cannot wrap
		// the residual-life comparison if height updates race the
		// earlier swappability filter.
		left := make([]int64, len(candidate))
		for i, index := range candidate {
			left[i] = deposits[index].ConfirmationHeight +
				int64(csvExpiry) - int64(blockHeight)
		}

		right := make([]int64, len(bestSelection))
		for i, index := range bestSelection {
			right[i] = deposits[index].ConfirmationHeight +
				int64(csvExpiry) - int64(blockHeight)
		}

		slices.Sort(left)
		slices.Sort(right)

		for i := range left {
			if left[i] == right[i] {
				continue
			}

			return left[i] < right[i]
		}

		return false
	}

	// search explores include/exclude choices. The branch-and-bound checks
	// are intentionally conservative: they only prune when no combination
	// below the current node can beat the best known total or tie it with a
	// smaller deposit count.
	var search func(index int, total btcutil.Amount, selected []int)
	search = func(index int, total btcutil.Amount, selected []int) {
		if total > maxAmount {
			return
		}

		if total >= minAmount && betterSelection(selected, total) {
			bestTotal = total
			bestSelection = append([]int(nil), selected...)
		}

		if index == len(deposits) {
			return
		}

		maxReachable := total + suffixSums[index]
		if maxReachable < bestTotal {
			return
		}

		if maxReachable == bestTotal && bestSelection != nil &&
			len(selected) >= len(bestSelection) {

			return
		}

		// The include branch must not reuse selected's backing array.
		// Otherwise a later append can leak into the exclude branch
		// when the slice still has spare capacity.
		selectedWithIndex := make([]int, len(selected)+1)
		copy(selectedWithIndex, selected)
		selectedWithIndex[len(selected)] = index

		search(
			index+1, total+deposits[index].Value,
			selectedWithIndex,
		)
		search(index+1, total, selected)
	}

	search(0, 0, nil)

	if len(bestSelection) == 0 {
		return nil, ErrNoAutoloopCandidate
	}

	selectedDeposits := make([]*deposit.Deposit, 0, len(bestSelection))
	for _, index := range bestSelection {
		selectedDeposits = append(selectedDeposits, deposits[index])
	}

	return selectedDeposits, nil
}
