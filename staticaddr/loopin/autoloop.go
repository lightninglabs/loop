package loopin

import (
	"context"
	"errors"

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

	err := m.cfg.DepositManager.EnsureDepositsFresh(ctx)
	if err != nil {
		return nil, 0, false, err
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

	return selectNoChangeDepositsWithMemoryBudget(
		maxAmount, minAmount, unfilteredDeposits, csvExpiry,
		blockHeight, excludedOutpoints, autoloopDPMaxMemoryBytes,
	)
}
