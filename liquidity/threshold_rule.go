package liquidity

import (
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/loop/swap"
)

var (
	// errInvalidLiquidityThreshold is returned when a liquidity threshold
	// has an invalid value.
	errInvalidLiquidityThreshold = errors.New("liquidity threshold must " +
		"be in [0:100)")

	// errInvalidThresholdSum is returned when the sum of the percentages
	// provided for a threshold rule is >= 100.
	errInvalidThresholdSum = errors.New("sum of incoming and outgoing " +
		"percentages must be < 100")
)

// SwapRule is a liquidity rule with a specific swap type.
type SwapRule struct {
	*ThresholdRule
	swap.Type
}

// ThresholdRule is a liquidity rule that implements minimum incoming and
// outgoing liquidity threshold.
type ThresholdRule struct {
	// MinimumIncoming is the percentage of incoming liquidity that we do
	// not want to drop below.
	MinimumIncoming int

	// MinimumOutgoing is the percentage of outgoing liquidity that we do
	// not want to drop below.
	MinimumOutgoing int
}

// NewThresholdRule returns a new threshold rule.
func NewThresholdRule(minIncoming, minOutgoing int) *ThresholdRule {
	return &ThresholdRule{
		MinimumIncoming: minIncoming,
		MinimumOutgoing: minOutgoing,
	}
}

// String returns a string representation of a rule.
func (r *ThresholdRule) String() string {
	return fmt.Sprintf("threshold rule: minimum incoming: %v%%, minimum "+
		"outgoing: %v%%", r.MinimumIncoming, r.MinimumOutgoing)
}

// validate validates the parameters that a rule was created with.
func (r *ThresholdRule) validate() error {
	if r.MinimumIncoming < 0 || r.MinimumIncoming > 100 {
		return errInvalidLiquidityThreshold
	}

	if r.MinimumOutgoing < 0 || r.MinimumOutgoing > 100 {
		return errInvalidLiquidityThreshold
	}

	if r.MinimumIncoming+r.MinimumOutgoing >= 100 {
		return errInvalidThresholdSum
	}

	return nil
}

// swapAmount suggests a swap based on the liquidity thresholds configured,
// returning zero if no swap is recommended.
func (r *ThresholdRule) swapAmount(channel *balances,
	restrictions *Restrictions, swapType swap.Type) btcutil.Amount {

	var (
		// For loop out swaps, we want to adjust our incoming liquidity
		// so the channel's incoming balance is our target.
		targetBalance = channel.incoming

		// For loop out swaps, we target a minimum amount of incoming
		// liquidity, so the minimum incoming threshold is our target
		// percentage.
		targetPercentage = uint64(r.MinimumIncoming)

		// For loop out swaps, we may want to preserve some of our
		// outgoing balance, so the channel's outgoing balance is our
		// reserve.
		reserveBalance = channel.outgoing

		// For loop out swaps, we may want to preserve some percentage
		// of our outgoing balance, so the minimum outgoing threshold
		// is our reserve percentage.
		reservePercentage = uint64(r.MinimumOutgoing)
	)

	// For loop in swaps, we reverse our target and reserve values.
	if swapType == swap.TypeIn {
		targetBalance = channel.outgoing
		targetPercentage = uint64(r.MinimumOutgoing)
		reserveBalance = channel.incoming
		reservePercentage = uint64(r.MinimumIncoming)
	}

	// Examine our total balance and required ratios to decide whether we
	// need to swap.
	amount := calculateSwapAmount(
		targetBalance, reserveBalance, channel.capacity,
		targetPercentage, reservePercentage,
	)

	// Limit our swap amount by the minimum/maximum thresholds set.
	switch {
	case amount < restrictions.Minimum:
		return 0

	case amount > restrictions.Maximum:
		return restrictions.Maximum

	default:
		return amount
	}
}

// calculateSwapAmount calculates amount for a swap based on thresholds.
// This function can be used for loop out or loop in, but the concept is the
// same - we want liquidity in one (target) direction, while preserving some
// minimum in the other (reserve) direction.
//   - target: this is the side of the channel(s) where we want to acquire some
//     liquidity. We aim for this liquidity to reach the threshold amount set.
//   - reserve: this is the side of the channel(s) that we will move liquidity
//     away from. This may not drop below a certain reserve threshold.
func calculateSwapAmount(targetAmount, reserveAmount,
	capacity btcutil.Amount, targetThresholdPercentage,
	reserveThresholdPercentage uint64) btcutil.Amount {

	targetGoal := btcutil.Amount(
		uint64(capacity) * targetThresholdPercentage / 100,
	)

	reserveMinimum := btcutil.Amount(
		uint64(capacity) * reserveThresholdPercentage / 100,
	)

	switch {
	// If we have sufficient target capacity, we do not need to swap.
	case targetAmount >= targetGoal:
		return 0

	// If we are already below the threshold set for reserve capacity, we
	// cannot take any further action.
	case reserveAmount <= reserveMinimum:
		return 0
	}

	// Express our minimum reserve amount as a maximum target amount.
	// We will use this value to limit the amount that we swap, so that we
	// do not dip below our reserve threshold.
	maximumTarget := capacity - reserveMinimum

	// Calculate the midpoint between our minimum and maximum target values.
	// We will aim to swap this amount so that we do not tip our reserve
	// balance beneath the desired level.
	midpoint := (targetGoal + maximumTarget) / 2

	// Calculate the amount of target balance we need to shift to reach
	// this desired midpoint.
	required := midpoint - targetAmount

	// Since we can have pending htlcs on our channel, we check the amount
	// of reserve capacity that we can shift before we fall below our
	// threshold.
	available := reserveAmount - reserveMinimum

	// If we do not have enough balance available to reach our midpoint, we
	// take no action. This is the case when we have a large portion of
	// pending htlcs.
	if available < required {
		return 0
	}

	return required
}
