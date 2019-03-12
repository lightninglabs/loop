package loopdb

// SwapState indicates the current state of a swap.
type SwapState uint8

const (
	// StateInitiated is the initial state of a swap. At that point, the
	// initiation call to the server has been made and the payment process
	// has been started for the swap and prepayment invoices.
	StateInitiated SwapState = 0

	// StatePreimageRevealed is reached when the sweep tx publication is
	// first attempted. From that point on, we should consider the preimage
	// to no longer be secret and we need to do all we can to get the sweep
	// confirmed. This state will mostly coalesce with StateHtlcConfirmed,
	// except in the case where we wait for fees to come down before we
	// sweep.
	StatePreimageRevealed = 1

	// StateSuccess is the final swap state that is reached when the sweep
	// tx has the required confirmation depth (SweepConfDepth) and the
	// server pulled the off-chain htlc.
	StateSuccess = 2

	// StateFailOffchainPayments indicates that it wasn't possible to find routes
	// for one or both of the off-chain payments to the server that
	// satisfied the payment restrictions (fee and timelock limits).
	StateFailOffchainPayments = 3

	// StateFailTimeout indicates that the on-chain htlc wasn't confirmed before
	// its expiry or confirmed too late (MinPreimageRevealDelta violated).
	StateFailTimeout = 4

	// StateFailSweepTimeout indicates that the on-chain htlc wasn't swept before
	// the server revoked the htlc. The server didn't pull the off-chain
	// htlc (even though it could have) and we timed out the off-chain htlc
	// ourselves. No funds lost.
	StateFailSweepTimeout = 5

	// StateFailInsufficientValue indicates that the published on-chain htlc had
	// a value lower than the requested amount.
	StateFailInsufficientValue = 6

	// StateFailTemporary indicates that the swap cannot progress because
	// of an internal error. This is not a final state. Manual intervention
	// (like a restart) is required to solve this problem.
	StateFailTemporary = 7

	// StateHtlcPublished means that the client published the on-chain htlc.
	StateHtlcPublished = 8
)

// SwapStateType defines the types of swap states that exist. Every swap state
// defined as type SwapState above, falls into one of these SwapStateType
// categories.
type SwapStateType uint8

const (
	// StateTypePending indicates that the swap is still pending.
	StateTypePending SwapStateType = 0

	// StateTypeSuccess indicates that the swap has completed successfully.
	StateTypeSuccess = 1

	// StateTypeFail indicates that the swap has failed.
	StateTypeFail = 2
)

// Type returns the type of the SwapState it is called on.
func (s SwapState) Type() SwapStateType {
	if s == StateInitiated || s == StateHtlcPublished ||
		s == StatePreimageRevealed || s == StateFailTemporary {

		return StateTypePending
	}

	if s == StateSuccess {
		return StateTypeSuccess
	}

	return StateTypeFail
}

// String returns a string representation of the swap's state.
func (s SwapState) String() string {
	switch s {
	case StateInitiated:
		return "Initiated"

	case StatePreimageRevealed:
		return "PreimageRevealed"

	case StateHtlcPublished:
		return "HtlcPublished"

	case StateSuccess:
		return "Success"

	case StateFailOffchainPayments:
		return "FailOffchainPayments"

	case StateFailTimeout:
		return "FailTimeout"

	case StateFailSweepTimeout:
		return "FailSweepTimeout"

	case StateFailInsufficientValue:
		return "FailInsufficientValue"

	case StateFailTemporary:
		return "FailTemporary"

	default:
		return "Unknown"
	}
}
