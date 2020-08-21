package liquidity

import (
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// balances summarizes the state of the balances on our node. Channel reserve,
// fees and pending htlc balances are not included in these balances.
type balances struct {
	// capacity is the total capacity in all of our channels.
	capacity btcutil.Amount

	// incoming is the total remote balance across all channels.
	incoming btcutil.Amount

	// outgoing is the total local balance across all channels.
	outgoing btcutil.Amount

	// channelID is short channel id of channel that has this set of
	// balances.
	channelID lnwire.ShortChannelID

	// pubkey is the public key of the peer we have the channel open with.
	pubkey route.Vertex
}
