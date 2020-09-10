package server

import (
	"time"

	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/routing/route"
)

// InRequest contains the required parameters for the swap.
type InRequest struct {
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

// LastUpdate returns the last update time of the swap
func (s *In) LastUpdate() time.Time {
	return s.LastUpdateTime
}

// SwapHash returns the swap hash.
func (s *In) SwapHash() lntypes.Hash {
	return s.Hash
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

	// HtlcAddressP2WSH contains the native segwit swap htlc address,
	// where the loop-in funds may be paid.
	HtlcAddressP2WSH btcutil.Address

	// HtlcAddressNP2WSH contains the nested segwit swap htlc address,
	// where the loop-in funds may be paid.
	HtlcAddressNP2WSH btcutil.Address

	// ServerMessages is the human-readable message received from the loop
	// server.
	ServerMessage string
}

// NewLoopInResponse contains information about a new loop in swap.
type NewLoopInResponse struct {
	// ReceiverKey is the key used for the receiver.
	ReceiverKey [33]byte

	// Expiry is the expiry of the swap in blocks.
	Expiry int32

	// ServerMessage is a message from the server.
	ServerMessage string
}
