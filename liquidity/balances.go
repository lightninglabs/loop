package liquidity

import (
	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// balances summarizes the state of the balances of a channel. Channel reserve,
// fees and pending htlc balances are not included in these balances.
type balances struct {
	// capacity is the total capacity of the channel.
	capacity btcutil.Amount

	// incoming is the remote balance of the channel.
	incoming btcutil.Amount

	// outgoing is the local balance of the channel.
	outgoing btcutil.Amount

	// channels is the channel that has these balances represent. This may
	// be more than one channel in the case where we are examining a peer's
	// liquidity as a whole.
	channels []lnwire.ShortChannelID

	// pubkey is the public key of the peer we have this balances set with.
	pubkey route.Vertex
}

// newBalances creates a balances struct from lndclient channel information.
func newBalances(info lndclient.ChannelInfo) *balances {
	return &balances{
		capacity: info.Capacity,
		incoming: info.RemoteBalance,
		outgoing: info.LocalBalance,
		channels: []lnwire.ShortChannelID{
			lnwire.NewShortChanIDFromInt(info.ChannelID),
		},
		pubkey: info.PubKeyBytes,
	}
}
