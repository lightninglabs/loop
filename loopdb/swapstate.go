package loopdb

import (
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
)

// SwapState indicates the current state of a swap. This enumeration is the
// union of loop in and loop out states. A single type is used for both swap
// types to be able to reduce code duplication that would otherwise be required.
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
	StatePreimageRevealed SwapState = 1

	// StateSuccess is the final swap state that is reached when the sweep
	// tx has the required confirmation depth (SweepConfDepth) and the
	// server pulled the off-chain htlc.
	StateSuccess SwapState = 2

	// StateFailOffchainPayments indicates that it wasn't possible to find
	// routes for one or both of the off-chain payments to the server that
	// satisfied the payment restrictions (fee and timelock limits).
	StateFailOffchainPayments SwapState = 3

	// StateFailTimeout indicates that the on-chain htlc wasn't confirmed
	// before its expiry or confirmed too late (MinPreimageRevealDelta
	// violated).
	StateFailTimeout SwapState = 4

	// StateFailSweepTimeout indicates that the on-chain htlc wasn't swept
	// before the server revoked the htlc. The server didn't pull the
	// off-chain htlc (even though it could have) and we timed out the
	// off-chain htlc ourselves. No funds lost.
	StateFailSweepTimeout SwapState = 5

	// StateFailInsufficientValue indicates that the published on-chain htlc
	// had a value lower than the requested amount.
	StateFailInsufficientValue SwapState = 6

	// StateFailTemporary indicates that the swap cannot progress because
	// of an internal error. This is not a final state. Manual intervention
	// (like a restart) is required to solve this problem.
	StateFailTemporary SwapState = 7

	// StateHtlcPublished means that the client published the on-chain htlc.
	StateHtlcPublished SwapState = 8

	// StateInvoiceSettled means that the swap invoice has been paid by the
	// server.
	StateInvoiceSettled SwapState = 9

	// StateFailIncorrectHtlcAmt indicates that the amount of an externally
	// published loop in htlc didn't match the swap amount.
	StateFailIncorrectHtlcAmt SwapState = 10

	// StateFailAbandoned indicates that a swap has been abandoned. Its
	// execution has been canceled. It won't further be processed.
	StateFailAbandoned SwapState = 11

	// StateFailInsufficientConfirmedBalance indicates that the swap wasn't
	// published due to insufficient confirmed balance.
	StateFailInsufficientConfirmedBalance SwapState = 12
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
		s == StatePreimageRevealed || s == StateFailTemporary ||
		s == StateInvoiceSettled {

		return StateTypePending
	}

	if s == StateSuccess {
		return StateTypeSuccess
	}

	return StateTypeFail
}

// IsPending returns true if the swap is in a pending state.
func (s SwapState) IsPending() bool {
	return s == StateInitiated || s == StateHtlcPublished ||
		s == StatePreimageRevealed || s == StateFailTemporary ||
		s == StateInvoiceSettled
}

// IsFinal returns true if the swap is in a final state.
func (s SwapState) IsFinal() bool {
	return !s.IsPending()
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

	case StateInvoiceSettled:
		return "InvoiceSettled"

	case StateFailIncorrectHtlcAmt:
		return "IncorrectHtlcAmt"

	case StateFailAbandoned:
		return "FailAbandoned"

	case StateFailInsufficientConfirmedBalance:
		return "InsufficientConfirmedBalance"

	default:
		return "Unknown"
	}
}

// SwapCost is a breakdown of the final swap costs.
type SwapCost struct {
	// Swap is the amount paid to the server.
	Server btcutil.Amount

	// Onchain is the amount paid to miners for the onchain tx.
	Onchain btcutil.Amount

	// Offchain is the amount paid in routing fees.
	Offchain btcutil.Amount
}

// Total returns the total costs represented by swap costs.
func (s SwapCost) Total() btcutil.Amount {
	return s.Server + s.Onchain + s.Offchain
}

// SwapStateData is all persistent data to describe the current swap state.
type SwapStateData struct {
	// SwapState is the state the swap is in.
	State SwapState

	// Cost are the accrued (final) costs so far.
	Cost SwapCost

	// HtlcTxHash is the tx id of the confirmed htlc.
	HtlcTxHash *chainhash.Hash
}
