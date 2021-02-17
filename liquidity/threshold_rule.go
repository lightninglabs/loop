package liquidity

import (
	"errors"
	"fmt"

	"github.com/btcsuite/btcutil"
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
	outRestrictions *Restrictions) btcutil.Amount {

	// Examine our total balance and required ratios to decide whether we
	// need to swap.
	amount := loopOutSwapAmount(
		channel, r.MinimumIncoming, r.MinimumOutgoing,
	)

	// Limit our swap amount by the minimum/maximum thresholds set.
	switch {
	case amount < outRestrictions.Minimum:
		return 0

	case amount > outRestrictions.Maximum:
		return outRestrictions.Maximum

	default:
		return amount
	}
}

// loopOutSwapAmount determines whether we can perform a loop out swap, and
// returns the amount we need to swap to reach the desired liquidity balance
// specified by the incoming and outgoing thresholds.
func loopOutSwapAmount(balances *balances, incomingThresholdPercent,
	outgoingThresholdPercent int) btcutil.Amount {

	minimumIncoming := btcutil.Amount(uint64(
		balances.capacity) *
		uint64(incomingThresholdPercent) / 100,
	)

	minimumOutgoing := btcutil.Amount(
		uint64(balances.capacity) *
			uint64(outgoingThresholdPercent) / 100,
	)

	switch {
	// If we have sufficient incoming capacity, we do not need to loop out.
	case balances.incoming >= minimumIncoming:
		return 0

	// If we are already below the threshold set for outgoing capacity, we
	// cannot take any further action.
	case balances.outgoing <= minimumOutgoing:
		return 0

	}

	// Express our minimum outgoing amount as a maximum incoming amount.
	// We will use this value to limit the amount that we swap, so that we
	// do not dip below our outgoing threshold.
	maximumIncoming := balances.capacity - minimumOutgoing

	// Calculate the midpoint between our minimum and maximum incoming
	// values. We will aim to swap this amount so that we do not tip our
	// outgoing balance beneath the desired level.
	midpoint := (minimumIncoming + maximumIncoming) / 2

	// Calculate the amount of incoming balance we need to shift to reach
	// this desired midpoint.
	required := midpoint - balances.incoming

	// Since we can have pending htlcs on our channel, we check the amount
	// of outbound capacity that we can shift before we fall below our
	// threshold.
	available := balances.outgoing - minimumOutgoing

	// If we do not have enough balance available to reach our midpoint, we
	// take no action. This is the case when we have a large portion of
	// pending htlcs.
	if available < required {
		return 0
	}

	return required
}
