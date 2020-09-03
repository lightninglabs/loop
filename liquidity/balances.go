package liquidity

import (
	"github.com/btcsuite/btcutil"
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
}
