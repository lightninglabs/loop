package liquidity

import (
	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/loop"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// Compile-time assertion that loopOutSwapSuggestion satisfies the
// swapSuggestion interface.
var _ swapSuggestion = (*loopOutSwapSuggestion)(nil)

// loopOutSwapSuggestion is an implementation of the swapSuggestion interface
// implemented to allow us to abstract away from the details of a specific
// swap.
type loopOutSwapSuggestion struct {
	loop.OutRequest
}

// amount returns the amount being swapped.
func (l *loopOutSwapSuggestion) amount() btcutil.Amount {
	return l.OutRequest.Amount
}

// fees returns the maximum fees we could possibly pay for this swap.
func (l *loopOutSwapSuggestion) fees() btcutil.Amount {
	return worstCaseOutFees(
		l.OutRequest.MaxPrepayRoutingFee, l.OutRequest.MaxSwapRoutingFee,
		l.OutRequest.MaxSwapFee, l.OutRequest.MaxMinerFee,
	)
}

// channels returns the set of channels the loop out swap is restricted to.
func (l *loopOutSwapSuggestion) channels() []lnwire.ShortChannelID {
	channels := make(
		[]lnwire.ShortChannelID, len(l.OutRequest.OutgoingChanSet),
	)

	for i, id := range l.OutRequest.OutgoingChanSet {
		channels[i] = lnwire.NewShortChanIDFromInt(id)
	}

	return channels
}

// peers returns the set of peers that the loop out swap is restricted to.
func (l *loopOutSwapSuggestion) peers(
	knownChans map[uint64]route.Vertex) []route.Vertex {

	peers := make(map[route.Vertex]struct{}, len(knownChans))

	for _, channel := range l.OutRequest.OutgoingChanSet {
		peer, ok := knownChans[channel]
		if !ok {
			log.Warnf("peer for channel: %v unknown", channel)
		}

		peers[peer] = struct{}{}
	}

	peerList := make([]route.Vertex, 0, len(peers))
	for peer := range peers {
		peerList = append(peerList, peer)
	}

	return peerList
}
