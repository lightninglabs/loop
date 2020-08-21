package liquidity

import (
	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/loop/swap"
)

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
