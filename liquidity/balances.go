package liquidity

import (
	"github.com/btcsuite/btcutil"
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

	// channelID is the channel that has these balances.
	channelID lnwire.ShortChannelID

	// pubkey is the public key of the peer we have this balances set with.
	pubkey route.Vertex
}

// newBalances creates a balances struct from lndclient channel information.
func newBalances(info lndclient.ChannelInfo) *balances {
	return &balances{
		capacity:  info.Capacity,
		incoming:  info.RemoteBalance,
		outgoing:  info.LocalBalance,
		channelID: lnwire.NewShortChanIDFromInt(info.ChannelID),
		pubkey:    info.PubKeyBytes,
	}
}
