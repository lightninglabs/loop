package server

import (
	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightningnetwork/lnd/lntypes"
)

// LoopOutTerms are the server terms on which it executes swaps.
type LoopOutTerms struct {
	// MinSwapAmount is the minimum amount that the server requires for a
	// swap.
	MinSwapAmount btcutil.Amount

	// MaxSwapAmount is the maximum amount that the server accepts for a
	// swap.
	MaxSwapAmount btcutil.Amount

	// MinCltvDelta is the minimum expiry delta for loop out swaps.
	MinCltvDelta int32

	// MaxCltvDelta is the maximum expiry delta for loop out swaps.
	MaxCltvDelta int32
}

// LoopOutQuote contains estimates for the fees making up the total swap cost
// for the client.
type LoopOutQuote struct {
	// SwapFee is the fee that the swap server is charging for the swap.
	SwapFee btcutil.Amount

	// PrepayAmount is the part of the swap fee that is requested as a
	// prepayment.
	PrepayAmount btcutil.Amount

	// MinerFee is an estimate of the on-chain fee that needs to be paid to
	// sweep the htlc.
	MinerFee btcutil.Amount

	// SwapPaymentDest is the node pubkey where to swap payment needs to be
	// sent to.
	SwapPaymentDest [33]byte
}

// LoopOutSwapInfo contains essential information of a loop-out swap after the
// swap is initiated.
type LoopOutSwapInfo struct { // nolint:golint
	// SwapHash contains the sha256 hash of the swap preimage.
	SwapHash lntypes.Hash

	// HtlcAddressP2WSH contains the native segwit swap htlc address that
	// the server will publish to.
	HtlcAddressP2WSH btcutil.Address

	// ServerMessages is the human-readable message received from the loop
	// server.
	ServerMessage string
}

// Out contains the full details of a loop out request. This includes things
// like the payment hash, the total value, and the final CTLV delay of the
// swap. We'll use this to track an active swap throughout that various swap
// stages.
type Out struct {
	// LoopOutContract describes the details of this loop.Out. Using these
	// details,the full swap can be executed.
	loopdb.LoopOutContract

	// State is the current state of the target swap.
	State loopdb.SwapState

	// SwapInfoKit contains shared data amongst all swap types.
	SwapInfoKit
}

// NewLoopOutResponse contains information about a new loop out swap.
type NewLoopOutResponse struct {
	// SwapInvoice is the invoice to be paid for the main swap amount.
	SwapInvoice string

	// PrepayInvoice is the invoice for the swap prepayment.
	PrepayInvoice string

	// SenderKey is the key for the sender.
	SenderKey [33]byte

	// ServerMessage is a message from the server.
	ServerMessage string
}
