package loop

import (
	"time"

	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/routing/route"
)

// OutRequest contains the required parameters for a loop out swap.
type OutRequest struct {
	// Amount specifies the requested swap amount in sat. This does not
	// include the swap and miner fee.
	Amount btcutil.Amount

	// Destination address for the swap.
	DestAddr btcutil.Address

	// MaxSwapRoutingFee is the maximum off-chain fee in msat that may be
	// paid for payment to the server. This limit is applied during path
	// finding. Typically this value is taken from the response of the
	// LoopOutQuote call.
	MaxSwapRoutingFee btcutil.Amount

	// MaxPrepayRoutingFee is the maximum off-chain fee in msat that may be
	// paid for payment to the server. This limit is applied during path
	// finding. Typically this value is taken from the response of the
	// LoopOutQuote call.
	MaxPrepayRoutingFee btcutil.Amount

	// MaxSwapFee is the maximum we are willing to pay the server for the
	// swap. This value is not disclosed in the swap initiation call, but
	// if the server asks for a higher fee, we abort the swap. Typically
	// this value is taken from the response of the LoopOutQuote call. It
	// includes the prepay amount.
	MaxSwapFee btcutil.Amount

	// MaxPrepayAmount is the maximum amount of the swap fee that may be
	// charged as a prepayment.
	MaxPrepayAmount btcutil.Amount

	// MaxMinerFee is the maximum in on-chain fees that we are willing to
	// spent. If we want to sweep the on-chain htlc and the fee estimate
	// turns out higher than this value, we cancel the swap. If the fee
	// estimate is lower, we publish the sweep tx.
	//
	// If the sweep tx isn't confirmed, we are forced to ratchet up fees
	// until it is swept. Possibly even exceeding MaxMinerFee if we get
	// close to the htlc timeout. Because the initial publication revealed
	// the preimage, we have no other choice. The server may already have
	// pulled the off-chain htlc. Only when the fee becomes higher than the
	// swap amount, we can only wait for fees to come down and hope - if we
	// are past the timeout - that the server isn't publishing the
	// revocation.
	//
	// MaxMinerFee is typically taken from the response of the
	// LoopOutQuote call.
	MaxMinerFee btcutil.Amount

	// SweepConfTarget specifies the targeted confirmation target for the
	// client sweep tx.
	SweepConfTarget int32

	// LoopOutChannel optionally specifies the short channel id of the
	// channel to loop out.
	LoopOutChannel *uint64

	// SwapPublicationDeadline can be set by the client to allow the server
	// delaying publication of the swap HTLC to save on chain fees.
	SwapPublicationDeadline time.Time
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

// LoopOutQuoteRequest specifies the swap parameters for which a quote is
// requested.
type LoopOutQuoteRequest struct {
	// Amount specifies the requested swap amount in sat. This does not
	// include the swap and miner fee.
	Amount btcutil.Amount

	// SweepConfTarget specifies the targeted confirmation target for the
	// client sweep tx.
	SweepConfTarget int32

	// SwapPublicationDeadline can be set by the client to allow the server
	// delaying publication of the swap HTLC to save on chain fees.
	SwapPublicationDeadline time.Time

	// TODO: Add argument to specify confirmation target for server
	// publishing htlc. This may influence the swap fee quote, because the
	// server needs to pay more for faster confirmations.
	//
	// TODO: Add arguments to specify maximum total time locks for the
	// off-chain swap payment and prepayment. This may influence the
	// available routes and off-chain fee estimates. To apply these maximum
	// values properly, the server needs to be queried for its required
	// final cltv delta values for the off-chain payments.
}

// LoopOutTerms are the server terms on which it executes swaps.
type LoopOutTerms struct {
	// MinSwapAmount is the minimum amount that the server requires for a
	// swap.
	MinSwapAmount btcutil.Amount

	// MaxSwapAmount is the maximum amount that the server accepts for a
	// swap.
	MaxSwapAmount btcutil.Amount
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

	// Time lock delta relative to current block height that swap server
	// will accept on the swap initiation call.
	CltvDelta int32

	// SwapPaymentDest is the node pubkey where to swap payment needs to be
	// sent to.
	SwapPaymentDest [33]byte
}

// LoopInRequest contains the required parameters for the swap.
type LoopInRequest struct {
	// Amount specifies the requested swap amount in sat. This does not
	// include the swap and miner fee.
	Amount btcutil.Amount

	// MaxSwapFee is the maximum we are willing to pay the server for the
	// swap. This value is not disclosed in the swap initiation call, but if
	// the server asks for a higher fee, we abort the swap. Typically this
	// value is taken from the response of the LoopInQuote call. It
	// includes the prepay amount.
	MaxSwapFee btcutil.Amount

	// MaxMinerFee is the maximum in on-chain fees that we are willing to
	// spent. If we publish the on-chain htlc and the fee estimate turns out
	// higher than this value, we cancel the swap.
	//
	// MaxMinerFee is typically taken from the response of the LoopInQuote
	// call.
	MaxMinerFee btcutil.Amount

	// HtlcConfTarget specifies the targeted confirmation target for the
	// client htlc tx.
	HtlcConfTarget int32

	// LastHop optionally specifies the last hop to use for the loop in
	// payment.
	LastHop *route.Vertex

	// ExternalHtlc specifies whether the htlc is published by an external
	// source.
	ExternalHtlc bool
}

// LoopInTerms are the server terms on which it executes loop in swaps.
type LoopInTerms struct {
	// MinSwapAmount is the minimum amount that the server requires for a
	// swap.
	MinSwapAmount btcutil.Amount

	// MaxSwapAmount is the maximum amount that the server accepts for a
	// swap.
	MaxSwapAmount btcutil.Amount
}

// In contains status information for a loop in swap.
type In struct {
	loopdb.LoopInContract

	SwapInfoKit

	// State where the swap is in.
	State loopdb.SwapState
}

// LoopInQuoteRequest specifies the swap parameters for which a quote is
// requested.
type LoopInQuoteRequest struct {
	// Amount specifies the requested swap amount in sat. This does not
	// include the swap and miner fee.
	Amount btcutil.Amount

	// HtlcConfTarget specifies the targeted confirmation target for the
	// client sweep tx.
	HtlcConfTarget int32

	// ExternalHtlc specifies whether the htlc is published by an external
	// source.
	ExternalHtlc bool
}

// LoopInQuote contains estimates for the fees making up the total swap cost
// for the client.
type LoopInQuote struct {
	// SwapFee is the fee that the swap server is charging for the swap.
	SwapFee btcutil.Amount

	// MinerFee is an estimate of the on-chain fee that needs to be paid to
	// sweep the htlc.
	MinerFee btcutil.Amount

	// Time lock delta relative to current block height that swap server
	// will accept on the swap initiation call.
	CltvDelta int32
}

// LoopInSwapInfo contains essential information of a loop-in swap after the
// swap is initiated.
type LoopInSwapInfo struct { // nolint
	// SwapHash contains the sha256 hash of the swap preimage.
	SwapHash lntypes.Hash

	// HtlcAddress contains the swap htlc address, where the loop-in
	// funds will be paid.
	HtlcAddress btcutil.Address
}

// SwapInfoKit contains common swap info fields.
type SwapInfoKit struct {
	// Hash is the sha256 hash of the preimage that unlocks the htlcs. It
	// is used to uniquely identify this swap.
	Hash lntypes.Hash

	// LastUpdateTime is the time of the last update of this swap.
	LastUpdateTime time.Time
}

// SwapInfo exposes common info fields for loop in and loop out swaps.
type SwapInfo struct {
	loopdb.SwapStateData

	loopdb.SwapContract

	// LastUpdateTime is the time of the last state change.
	LastUpdate time.Time

	// SwapHash stores the swap preimage hash.
	SwapHash lntypes.Hash

	// SwapType describes whether this is a loop in or loop out swap.
	SwapType swap.Type

	// HtlcAddress holds the HTLC address of the swap.
	HtlcAddress btcutil.Address

	// ExternalHtlc is set to true for external loop-in swaps.
	ExternalHtlc bool
}

// LastUpdate returns the last update time of the swap
func (s *In) LastUpdate() time.Time {
	return s.LastUpdateTime
}

// SwapHash returns the swap hash.
func (s *In) SwapHash() lntypes.Hash {
	return s.Hash
}
