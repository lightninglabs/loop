package liquidity

import (
	"errors"
	"fmt"

	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

var (
	// ErrNoCapacity is returned when there is no capacity in the set of
	// channels provided.
	ErrNoCapacity = errors.New("no capacity available for swaps")
)

type channelDetails struct {
	amount  btcutil.Amount
	channel lnwire.ShortChannelID
	peer    route.Vertex
}

func (r *ThresholdRule) getSwaps(channels []balances, outRestrictions,
	inRestrictions *Restrictions) ([]SwapRecommendation, error) {

	// To decide whether we should swap, we will look at all of our balances
	// combined.
	var totalBalance balances
	for _, balance := range channels {
		totalBalance.capacity += balance.capacity
		totalBalance.incoming += balance.incoming
		totalBalance.outgoing += balance.outgoing
	}

	// Check that we have some balance in the set of channels provided.
	if totalBalance.capacity == 0 {
		return nil, ErrNoCapacity
	}

	// Examine our total balance and required ratios to decide whether we
	// need to swap.
	amount, swapType := shouldSwap(
		&totalBalance, r.MinimumInbound, r.MinimumOutbound,
	)

	// If a nil swap type is returned, we do not need to recommend any swaps
	// so we return an empty swap set.
	if swapType == nil {
		return nil, nil
	}

	var (
		restrictions *Restrictions
		makeSwap     swapRecommendationFunc
	)

	// Switch on our swap type to determine the amount that we need to swap
	// and set the appropriate set of restrictions.
	switch *swapType {
	case swap.TypeOut:
		restrictions = outRestrictions
		makeSwap = newLoopOutRecommendation

	case swap.TypeIn:
		restrictions = inRestrictions
		makeSwap = newLoopInRecommendation

	default:
		return nil, fmt.Errorf("unknown swap type: %v", *swapType)
	}

	// At this stage, we know that we need to perform a swap, and we know
	// the amount of our total capacity that we need to move. Before we
	// proceed, we do a quick check that the amount we need to move is more
	// than the minimum swap amount, returning if it is not.
	if amount < restrictions.MinimumAmount {
		return nil, nil
	}

	// Make a set of balances that contain the balance that should be used
	// to create a swap, depending on the type of swap we are performing.
	available := make([]channelDetails, len(channels))
	for i, channel := range channels {
		details := channelDetails{
			channel: channel.channelID,
			peer:    channel.pubkey,
		}

		if *swapType == swap.TypeOut {
			details.amount = channel.outgoing
		} else {
			details.amount = channel.incoming
		}

		available[i] = details
	}

	// TODO(carla): add multi-swap selection for loop out, mocking the
	// behaviour of lnd's current split algorithm.
	swaps := selectSwaps(
		available, makeSwap, amount, restrictions.MinimumAmount,
		restrictions.MaximumAmount,
	)

	return swaps, nil
}

// shouldSwap examines our current set of balances, and required thresholds and
// determines whether we can improve our liquidity balance. It returns a swap
// type which indicates the type of swap that should be executed (and nil if
// no swap is possible/required) and a reason enum which further explain the
// reasoning for this action.
func shouldSwap(balances *balances, minInbound,
	minOutbound int) (btcutil.Amount, *swap.Type) {

	// Create balance requirements for our inbound and outbound liquidity.
	inbound := balanceRequirement{
		currentAmount: balances.incoming,
		minimumAmount: btcutil.Amount(
			uint64(balances.capacity) * uint64(minInbound) / 100,
		),
	}

	outbound := balanceRequirement{
		currentAmount: balances.outgoing,
		minimumAmount: btcutil.Amount(
			uint64(balances.capacity) * uint64(minOutbound) / 100,
		),
	}

	switch {
	// If we have too little inbound and too little outbound, we cannot
	// do anything to help our liquidity situation. This will happen in the
	// case where we have a lot of pending htlcs on our channels.
	case inbound.hasDeficit() && outbound.hasDeficit():
		return 0, nil

	// If we have too little inbound, but not too little outbound, it is
	// possible that a loop out will improve our inbound liquidity.
	case inbound.hasDeficit() && outbound.hasSurplus():
		amt := calculateSwapAmount(balances.capacity, inbound, outbound)
		out := swap.TypeOut
		return amt, &out

	// If we have enough inbound, and too little outbound, it is possible
	// that we can loop in to improve our outbound liquidity.
	case inbound.hasSurplus() && outbound.hasDeficit():
		amt := calculateSwapAmount(balances.capacity, outbound, inbound)
		in := swap.TypeIn
		return amt, &in

	// If we have enough inbound and enough outbound, we do not need to
	// take any actions at present.
	default:
		return 0, nil
	}
}

// balanceRequirement describes liquidity in one direction.
type balanceRequirement struct {
	// currentAmount is the amount of liquidity have have in the direction.
	currentAmount btcutil.Amount

	// minimumAmount is the minimum amount of liquidity that we require in
	// this direction.
	minimumAmount btcutil.Amount
}

func (i balanceRequirement) hasSurplus() bool {
	return i.currentAmount > i.minimumAmount
}

func (i balanceRequirement) hasDeficit() bool {
	return i.currentAmount < i.minimumAmount
}

// calculateSwapAmount calculates the amount of capacity that we need to shift
// to improve the balance of our channels, and returns a reason which provides
// additional context. This function is used in the case where we have surplus
// liquidity in one direction, and deficit in another. This is the case in which
// we recommend swaps, if we have deficits on both sides, we cannot swap without
// further unbalancing, and if we have surplus on both sides, we do not need to.
//
// This function calculates the portion of our total capacity we should shift
// from the surplus side to deficit side without unbalancing the surplus side.
// This is important, because we do not want to recommend a swap in one
// direction that just results in our needing to produce a swap in the other
// direction.
func calculateSwapAmount(capacity btcutil.Amount, deficitSide,
	surplusSide balanceRequirement) btcutil.Amount {

	// Get the maximum shift allowed on our surplus side that is possible
	// before it dips beneath its minimum.
	available := surplusSide.currentAmount - surplusSide.minimumAmount

	// Get the midpoint between our two minimum amounts, expressed from
	// the perspective of the side that we currently have a deficit of
	// liquidity.
	midpoint := (deficitSide.minimumAmount +
		(capacity - surplusSide.minimumAmount)) / 2

	// Calculate the amount of balance we need to shift our
	// current deficit side to reach this midpoint.
	midpointDelta := midpoint - deficitSide.currentAmount

	// If the amount we have available is less than the midpoint between
	// our two minimums, we do not recommend any action. This may happen
	// if a large portion of our balance is in a pending state.
	// TODO(carla): do a best-effort split attempt here?
	if available < midpointDelta {
		return 0
	}

	// Otherwise, we have sufficient surplus to shift our deficit side all
	// the way to the midpoint. We return the amount we need to reach this
	// midpoint so that we do not overshoot and produce a deficit in the
	// opposite direction.
	return midpointDelta
}
