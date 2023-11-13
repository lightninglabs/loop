package loop

import (
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/zpay32"
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

	// Initiator is an optional string that identifies what software
	// initiated the swap (loop CLI, autolooper, LiT UI and so on) and is
	// appended to the user agent string.
	Initiator string
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

	// Initiator is an optional string that identifies what software
	// initiated the swap (loop CLI, autolooper, LiT UI and so on) and is
	// appended to the user agent string.
	Initiator string
}

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

	// Label contains an optional label for the swap.
	Label string

	// Initiator is an optional string that identifies what software
	// initiated the swap (loop CLI, autolooper, LiT UI and so on) and is
	// appended to the user agent string.
	Initiator string

	// Private indicates whether the destination node should be considered
	// private. In which case, loop will generate hophints to assist with
	// probing and payment.
	Private bool

	// RouteHints are optional route hints to reach the destination through
	// private channels.
	RouteHints [][]zpay32.HopHint
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

	// LastHop is an optional last hop to use. This last hop is used when
	// the client has already requested a server probe for more accurate
	// routing fee estimation.
	LastHop *route.Vertex

	// RouteHints are optional route hints to reach the destination through
	// private channels.
	RouteHints [][]zpay32.HopHint

	// Private indicates whether the destination node should be considered
	// private. In which case, loop will generate hophints to assist with
	// probing and payment.
	Private bool

	// Initiator is an optional string that identifies what software
	// initiated the swap (loop CLI, autolooper, LiT UI and so on) and is
	// appended to the user agent string.
	Initiator string
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

	// HtlcAddressP2WSH contains the native segwit swap htlc address,
	// where the loop-in funds may be paid.
	HtlcAddressP2WSH btcutil.Address

	// HtlcAddresP2TR contains the v3 (pay to taproot) htlc address.
	HtlcAddressP2TR btcutil.Address

	// ServerMessages is the human-readable message received from the loop
	// server.
	ServerMessage string
}

// LoopOutSwapInfo contains essential information of a loop-out swap after the
// swap is initiated.
type LoopOutSwapInfo struct { // nolint:revive
	// SwapHash contains the sha256 hash of the swap preimage.
	SwapHash lntypes.Hash

	// HtlcAddress contains the swap htlc address that the server will
	// publish to.
	HtlcAddress btcutil.Address

	// ServerMessages is the human-readable message received from the loop
	// server.
	ServerMessage string
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

	// HtlcAddressP2WSH stores the address of the P2WSH (native segwit)
	// swap htlc. This is used for both loop-in and loop-out.
	HtlcAddressP2WSH btcutil.Address

	// HtlcAddressP2TR stores the address of the P2TR (taproot) swap htlc.
	// This is used for both internal and external loop-in and loop out.
	HtlcAddressP2TR btcutil.Address

	// ExternalHtlc is set to true for external loop-in swaps.
	ExternalHtlc bool

	// LastHop optionally specifies the last hop to use for the loop in
	// payment. On a loop out this field is nil.
	LastHop *route.Vertex

	// OutgoingChanSet optionally specifies the short channel ids of the
	// channels that may be used to loop out. On a loop in this field
	// is nil.
	OutgoingChanSet loopdb.ChannelSet
}

// LastUpdate returns the last update time of the swap.
func (s *In) LastUpdate() time.Time {
	return s.LastUpdateTime
}

// SwapHash returns the swap hash.
func (s *In) SwapHash() lntypes.Hash {
	return s.Hash
}

// ProbeRequest specifies probe parameters for the server probe.
type ProbeRequest struct {
	// Amount is the amount that will be probed.
	Amount btcutil.Amount

	// LastHop is the last hop along the route.
	LastHop *route.Vertex

	// Optional hop hints.
	RouteHints [][]zpay32.HopHint
}

// AbandonSwapRequest specifies the swap to abandon. It is identified by its
// swap hash.
type AbandonSwapRequest struct {
	SwapHash lntypes.Hash
}
