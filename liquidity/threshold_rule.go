package liquidity

import (
	"errors"
	"fmt"
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
