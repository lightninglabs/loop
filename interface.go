package loop

import (
	"time"

	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightningnetwork/lnd/lntypes"
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

	// HtlcConfirmations specifies the number of confirmations we require
	// for on chain loop out htlcs.
	HtlcConfirmations int32

	// OutgoingChanSet optionally specifies the short channel ids of the
	// channels that may be used to loop out.
	OutgoingChanSet loopdb.ChannelSet

	// SwapPublicationDeadline can be set by the client to allow the server
	// delaying publication of the swap HTLC to save on chain fees.
	SwapPublicationDeadline time.Time

	// Expiry is the absolute expiry height of the on-chain htlc.
	Expiry int32

	// Label contains an optional label for the swap.
	Label string
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

	// HtlcAddressP2WSH stores the address of the P2WSH (native segwit)
	// swap htlc. This is used for both loop-in and loop-out.
	HtlcAddressP2WSH btcutil.Address

	// HtlcAddressNP2WSH stores the address of the NP2WSH (nested segwit)
	// swap htlc. This is only used for external loop-in.
	HtlcAddressNP2WSH btcutil.Address

	// ExternalHtlc is set to true for external loop-in swaps.
	ExternalHtlc bool
}
