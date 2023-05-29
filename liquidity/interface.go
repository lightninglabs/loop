package liquidity

import (
	"context"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// FeeLimit is an interface implemented by different strategies for limiting
// the fees we pay for autoloops.
type FeeLimit interface {
	// String returns the string representation of fee limits.
	String() string

	// validate returns an error if the values provided are invalid.
	validate() error

	// mayLoopOut checks whether we may dispatch a loop out swap based on
	// the current fee conditions.
	mayLoopOut(estimate chainfee.SatPerKWeight) error

	// loopOutLimits checks whether the quote provided is within our fee
	// limits for the swap amount.
	loopOutLimits(amount btcutil.Amount, quote *loop.LoopOutQuote) error

	// loopOutFees return the maximum prepay and invoice routing fees for
	// a swap amount and quote.
	loopOutFees(amount btcutil.Amount, quote *loop.LoopOutQuote) (
		btcutil.Amount, btcutil.Amount, btcutil.Amount)

	// loopInLimits checks whether the quote provided is within our fee
	// limits for the swap amount.
	loopInLimits(amount btcutil.Amount,
		quote *loop.LoopInQuote) error
}

// swapBuilder is an interface used to build our different swap types.
type swapBuilder interface {
	// swapType returns the swap type that the builder is responsible for
	// creating.
	swapType() swap.Type

	// maySwap checks whether we can currently execute a swap, examining
	// the current on-chain fee conditions against relevant to our swap
	// type against our fee restrictions.
	maySwap(ctx context.Context, params Parameters) error

	// inUse examines our current swap traffic to determine whether we
	// should suggest the builder's type of swap for the peer and channels
	// suggested.
	inUse(traffic *swapTraffic, peer route.Vertex,
		channels []lnwire.ShortChannelID) error

	// buildSwap creates a swap for the target peer/channels provided. The
	// autoloop boolean indicates whether this swap will actually be
	// executed, because there are some calls we can leave out if this swap
	// is just for a dry run.
	buildSwap(ctx context.Context, peer route.Vertex,
		channels []lnwire.ShortChannelID, amount btcutil.Amount,
		params Parameters) (swapSuggestion, error)
}

// swapSuggestion is an interface implemented by suggested swaps for our
// different swap types. This interface is used to allow us to handle different
// swap types with the same autoloop logic.
type swapSuggestion interface {
	// fees returns the highest possible fee amount we could pay for a swap
	// in satoshis.
	fees() btcutil.Amount

	// amount returns the swap amount in satoshis.
	amount() btcutil.Amount

	// channels returns the set of channels involved in the swap.
	channels() []lnwire.ShortChannelID

	// peers returns the set of peers involved in the swap, taking a map
	// of known channel IDs to peers as an argument so that channel peers
	// can be looked up.
	peers(knownChans map[uint64]route.Vertex) []route.Vertex
}
