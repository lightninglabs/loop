package liquidity

import (
	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/loop"
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
