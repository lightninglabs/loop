package liquidity

import (
	"errors"
	"fmt"
)

var (
	// ErrInvalidLiquidityThreshold is returned when a liquidity threshold
	// has an invalid value.
	ErrInvalidLiquidityThreshold = errors.New("liquidity threshold must " +
		"be in [0:100)")

	// ErrInvalidThresholdSum is returned when the sum of the percentages
	// provided for a threshold rule is >= 100.
	ErrInvalidThresholdSum = errors.New("sum of inbound and outbound " +
		"percentages must be < 100")
)

// Rule is an interface implemented by different liquidity rules that we can
// apply.
type Rule interface {
	fmt.Stringer

	// validate validates the parameters that a rule was created with.
	validate() error

	// getSwaps returns a set of recommended swaps for a set of channel
	// balances, taking into account the restrictions placed on swaps.
	getSwaps(channelBalances []balances, outRestrictions,
		inRestrictions *Restrictions) ([]SwapRecommendation, error)
}

// ThresholdRule is a liquidity rule that implements minimum incoming and
// outgoing liquidity threshold.
type ThresholdRule struct {
	// Minimum inbound is the minimum percentage of inbound liquidity we
	// allow before recommending a loop out to acquire incoming liquidity.
	MinimumInbound int

	// MinimumOutbound is the minimum percentage of outbound liquidity we
	// allow before recommending a loop in to acquire outgoing liquidity.
	MinimumOutbound int
}

// NewThresholdRule returns a new threshold rule.
func NewThresholdRule(minimumInbound, minimumOutbound int) *ThresholdRule {
	return &ThresholdRule{
		MinimumInbound:  minimumInbound,
		MinimumOutbound: minimumOutbound,
	}
}

// String returns a string representation of a rule.
func (r *ThresholdRule) String() string {
	return fmt.Sprintf("threshold rule: minimum inbound: %v%%, minimum "+
		"outbound: %v%%", r.MinimumInbound, r.MinimumOutbound)
}

// validate validates the parameters that a rule was created with.
func (r *ThresholdRule) validate() error {
	if r.MinimumInbound < 0 || r.MinimumInbound > 100 {
		return ErrInvalidLiquidityThreshold
	}

	if r.MinimumOutbound < 0 || r.MinimumOutbound > 100 {
		return ErrInvalidLiquidityThreshold
	}

	if r.MinimumInbound+r.MinimumOutbound >= 100 {
		return ErrInvalidThresholdSum
	}

	return nil
}

// ExcludePeer is a rule that excludes a target from consideration.
type ExcludeRule struct{}

// NewExcludePeerRule creates a new exclude peer rule.
func NewExcludeRule() *ExcludeRule {
	return &ExcludeRule{}
}

// String returns a string representation of a rule.
func (r *ExcludeRule) String() string {
	return fmt.Sprintf("exclude target from consideration")
}

// validate validates the parameters that a rule was created with.
func (r *ExcludeRule) validate() error {
	return nil
}

// getSwaps returns a set of recommended swaps for a set of channel
// balances, taking into account the restrictions placed on swaps.
func (r *ExcludeRule) getSwaps(_ []balances, _,
	_ *Restrictions) ([]SwapRecommendation, error) {

	return nil, nil
}