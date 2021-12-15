package liquidity

import "fmt"

// Reason is an enum which represents the various reasons we have for not
// executing a swap.
type Reason uint8

const (
	// ReasonNone is the zero value reason, added so that this enum can
	// align with the numeric values used in our protobufs and avoid
	// ambiguity around default zero values.
	ReasonNone Reason = iota

	// ReasonBudgetNotStarted indicates that we do not recommend any swaps
	// because the start time for our budget has not arrived yet.
	ReasonBudgetNotStarted

	// ReasonSweepFees indicates that the estimated fees to sweep swaps
	// are too high right now.
	ReasonSweepFees

	// ReasonBudgetElapsed indicates that the autoloop budget for the
	// period has been elapsed.
	ReasonBudgetElapsed

	// ReasonInFlight indicates that the limit on in-flight automatically
	// dispatched swaps has already been reached.
	ReasonInFlight

	// ReasonSwapFee indicates that the server fee for a specific swap is
	// too high.
	ReasonSwapFee

	// ReasonMinerFee indicates that the miner fee for a specific swap is
	// to high.
	ReasonMinerFee

	// ReasonPrepay indicates that the prepay fee for a specific swap is
	// too high.
	ReasonPrepay

	// ReasonFailureBackoff indicates that a swap has recently failed for
	// this target, and the backoff period has not yet passed.
	ReasonFailureBackoff

	// ReasonLoopOut indicates that a loop out swap is currently utilizing
	// the channel, so it is not eligible.
	ReasonLoopOut

	// ReasonLoopIn indicates that a loop in swap is currently in flight
	// for the peer, so it is not eligible.
	ReasonLoopIn

	// ReasonLiquidityOk indicates that a target meets the liquidity
	// balance expressed in its rule, so no swap is needed.
	ReasonLiquidityOk

	// ReasonBudgetInsufficient indicates that we cannot perform a swap
	// because we do not have enough pending budget available. This differs
	// from budget elapsed, because we still have some budget available,
	// but we have allocated it to other swaps.
	ReasonBudgetInsufficient

	// ReasonFeePPMInsufficient indicates that the fees a swap would require
	// are greater than the portion of swap amount allocated to fees.
	ReasonFeePPMInsufficient

	// ReasonLoopInUnreachable indicates that the server does not have a
	// path to the client, so cannot perform a loop in swap at this time.
	ReasonLoopInUnreachable
)

// String returns a string representation of a reason.
func (r Reason) String() string {
	switch r {
	case ReasonNone:
		return "none"

	case ReasonBudgetNotStarted:
		return "budget not started"

	case ReasonSweepFees:
		return "sweep fees to high"

	case ReasonBudgetElapsed:
		return "budget elapsed"

	case ReasonInFlight:
		return "autoloops already in flight"

	case ReasonSwapFee:
		return "swap server fee to high"

	case ReasonMinerFee:
		return "miner fee to high"

	case ReasonPrepay:
		return "prepayment too high"

	case ReasonFailureBackoff:
		return "backing off due to failure"

	case ReasonLoopOut:
		return "loop out using channel"

	case ReasonLoopIn:
		return "loop in using peer"

	case ReasonLiquidityOk:
		return "liquidity balance ok"

	case ReasonBudgetInsufficient:
		return "budget insufficient"

	case ReasonFeePPMInsufficient:
		return "fee portion insufficient"

	case ReasonLoopInUnreachable:
		return "loop in unreachable"

	default:
		return "unknown"
	}
}

// reasonError is an error type which embeds our reasons for not performing
// swaps.
type reasonError struct {
	reason Reason
}

func newReasonError(r Reason) *reasonError {
	return &reasonError{
		reason: r,
	}
}

// Error returns an error string for a reason error.
func (r *reasonError) Error() string {
	return fmt.Sprintf("swap reason: %v", r.reason)
}
